package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/huh/v2"
	"charm.land/huh/v2/spinner"
	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/auth"
	"github.com/InfrastryAI/infra/internal/config"
	"github.com/spf13/cobra"
)

func (command *CLI) initialize(cmd *cobra.Command) error {
	if command.initialized {
		return nil
	}
	command.store = config.Store{Path: command.configPath}
	loaded, err := command.store.Load()
	if err != nil {
		return err
	}
	command.configuration = loaded

	urlWasExplicit := command.dependencies.Getenv("INFRASTRY_API_URL") != "" || cmd.Root().PersistentFlags().Changed("api-url")
	requestedURL := command.apiURLFlag
	if !urlWasExplicit && loaded.Current != "" {
		requestedURL = loaded.Current
	}
	command.apiURL, err = config.NormalizeAPIURL(requestedURL)
	if err != nil {
		return usageError{err.Error()}
	}
	command.colors = !command.noColor && command.dependencies.Getenv("NO_COLOR") == "" && outputIsTerminal(command.dependencies.Stdout)

	migrated := command.hydrateCredentials()
	if migrated {
		if err := command.persistConfiguration(false); err != nil {
			return err
		}
	}
	command.initialized = true
	return nil
}

func (command *CLI) hydrateCredentials() bool {
	migrated := false
	for apiURL, profile := range command.configuration.Profiles {
		if profile.AccessToken != "" || profile.RefreshToken != "" {
			command.credentialSources[apiURL] = "file"
			credentials := config.Credentials{AccessToken: profile.AccessToken, RefreshToken: profile.RefreshToken}
			if err := command.dependencies.Credentials.Set(apiURL, credentials); err == nil {
				command.credentialSources[apiURL] = "keyring"
				migrated = true
			}
			continue
		}

		credentials, err := command.dependencies.Credentials.Get(apiURL)
		switch {
		case err == nil:
			profile.AccessToken = credentials.AccessToken
			profile.RefreshToken = credentials.RefreshToken
			command.configuration.Profiles[apiURL] = profile
			command.credentialSources[apiURL] = "keyring"
		case errors.Is(err, config.ErrCredentialsNotFound):
			command.credentialSources[apiURL] = "none"
		default:
			command.credentialSources[apiURL] = "keyring-error"
			command.credentialErrors[apiURL] = err
		}
	}
	return migrated
}

func (command *CLI) persistConfiguration(reportFallback bool) error {
	onDisk := command.configuration
	onDisk.Profiles = make(map[string]config.Profile, len(command.configuration.Profiles))
	for apiURL, profile := range command.configuration.Profiles {
		diskProfile := profile
		if profile.AccessToken != "" || profile.RefreshToken != "" {
			credentials := config.Credentials{AccessToken: profile.AccessToken, RefreshToken: profile.RefreshToken}
			if err := command.dependencies.Credentials.Set(apiURL, credentials); err == nil {
				command.credentialSources[apiURL] = "keyring"
				delete(command.credentialErrors, apiURL)
				diskProfile.AccessToken = ""
				diskProfile.RefreshToken = ""
			} else {
				command.credentialSources[apiURL] = "file"
				if reportFallback {
					fmt.Fprintf(command.dependencies.Stderr, "Warning: OS keyring unavailable (%s); credentials remain in %s with private file permissions.\n", err, command.store.Path)
				}
			}
		}
		onDisk.Profiles[apiURL] = diskProfile
	}
	return command.store.Save(onDisk)
}

func (command *CLI) authenticationError() error {
	if err := command.credentialErrors[command.apiURL]; err != nil {
		return fmt.Errorf("load authentication from the OS keyring: %w; rerun `infra auth login --super` or your restricted `--scope` login to restore the session", err)
	}
	return auth.ErrLoginRequired
}

func (command *CLI) canPrompt() bool {
	return !command.noInput && command.dependencies.IsTerminal()
}

func (command *CLI) selectTeam(ctx context.Context, teams []api.Team, title string) (api.Team, error) {
	if len(teams) == 0 {
		return api.Team{}, errors.New("no teams are available for this account; create a team in Infrastry and try again")
	}
	options := make([]huh.Option[string], 0, len(teams))
	for _, team := range teams {
		options = append(options, huh.NewOption(fmt.Sprintf("%s (%s)", team.Name, team.Ref), team.Ref))
	}
	selected := teams[0].Ref
	field := huh.NewSelect[string]().Title(title).Options(options...).Value(&selected).Filtering(true)
	form := huh.NewForm(huh.NewGroup(field)).
		WithInput(command.dependencies.Stdin).
		WithOutput(command.dependencies.Stderr).
		WithAccessible(command.dependencies.Getenv("INFRASTRY_ACCESSIBLE") != "")
	if err := form.RunWithContext(ctx); err != nil {
		return api.Team{}, err
	}
	return resolveTeam(teams, selected)
}

func (command *CLI) selectApp(ctx context.Context, apps []api.App, title string) (api.App, error) {
	if len(apps) == 0 {
		return api.App{}, errors.New("no applications are available in the selected team")
	}
	options := make([]huh.Option[string], 0, len(apps))
	for _, app := range apps {
		options = append(options, huh.NewOption(fmt.Sprintf("%s (%s)", app.Name, app.Ref), app.Ref))
	}
	selected := apps[0].Ref
	field := huh.NewSelect[string]().Title(title).Options(options...).Value(&selected).Filtering(true)
	form := huh.NewForm(huh.NewGroup(field)).
		WithInput(command.dependencies.Stdin).
		WithOutput(command.dependencies.Stderr).
		WithAccessible(command.dependencies.Getenv("INFRASTRY_ACCESSIBLE") != "")
	if err := form.RunWithContext(ctx); err != nil {
		return api.App{}, err
	}
	return resolveApp(apps, selected)
}

func (command *CLI) resolveTeam(ctx context.Context, teams []api.Team, selector string) (api.Team, error) {
	team, err := resolveTeam(teams, selector)
	if err == nil || !command.canPrompt() || !strings.Contains(err.Error(), "ambiguous") {
		return team, err
	}
	var matches []api.Team
	for _, candidate := range teams {
		if strings.EqualFold(candidate.Name, selector) {
			matches = append(matches, candidate)
		}
	}
	return command.selectTeam(ctx, matches, fmt.Sprintf("Select the team named %q", selector))
}

func (command *CLI) resolveApp(ctx context.Context, apps []api.App, selector string) (api.App, error) {
	app, err := resolveApp(apps, selector)
	if err == nil || !command.canPrompt() || !strings.Contains(err.Error(), "ambiguous") {
		return app, err
	}
	var matches []api.App
	for _, candidate := range apps {
		if strings.EqualFold(candidate.Name, selector) {
			matches = append(matches, candidate)
		}
	}
	return command.selectApp(ctx, matches, fmt.Sprintf("Select the application named %q", selector))
}

func (command *CLI) withSpinner(ctx context.Context, title string, enabled bool, action func(context.Context) error) error {
	if !enabled || !command.canPrompt() {
		return action(ctx)
	}
	return spinner.New().
		Title(title).
		WithInput(command.dependencies.Stdin).
		WithOutput(command.dependencies.Stderr).
		WithAccessible(command.dependencies.Getenv("INFRASTRY_ACCESSIBLE") != "").
		Context(ctx).
		ActionWithErr(action).
		Run()
}

func (command *CLI) completeConfigSet(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return []string{"api\tInfrastry installation URL", "team\tSelected team"}, cobra.ShellCompDirectiveNoFileComp
	}
	if len(args) != 1 || args[0] != "team" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if err := command.initialize(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	teams, err := command.listTeams(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	values := make([]string, 0, len(teams))
	for _, team := range teams {
		values = append(values, team.Ref+"\t"+team.Name)
	}
	return values, cobra.ShellCompDirectiveNoFileComp
}

func (command *CLI) completeApps(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return command.appCompletionValues(cmd)
}

func (command *CLI) appCompletionValues(cmd *cobra.Command) ([]string, cobra.ShellCompDirective) {
	if err := command.initialize(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	team, err := command.teamForCompletion(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := command.apiClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	apps, err := client.ListApps(ctx, api.ListAppsOptions{TeamSlug: team.Slug})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	values := make([]string, 0, len(apps)*2)
	for _, app := range apps {
		values = append(values, app.Slug+"\t"+app.Name)
		if app.Ref != app.Slug {
			values = append(values, app.Ref+"\t"+app.Name)
		}
	}
	return values, cobra.ShellCompDirectiveNoFileComp
}

func (command *CLI) teamForCompletion(ctx context.Context) (api.Team, error) {
	profile, ok := command.configuration.Profile(command.apiURL)
	if !ok || (profile.AccessToken == "" && profile.RefreshToken == "") {
		return api.Team{}, command.authenticationError()
	}
	if profile.TeamID != "" && profile.TeamSlug != "" && profile.TeamRef != "" {
		return api.Team{ID: profile.TeamID, Slug: profile.TeamSlug, Ref: profile.TeamRef, Name: profile.TeamName}, nil
	}
	teams, err := command.listTeams(ctx)
	if err != nil {
		return api.Team{}, err
	}
	for _, team := range teams {
		if (profile.TeamRef != "" && team.Ref == profile.TeamRef) ||
			(profile.TeamRef == "" && profile.TeamSlug != "" && team.Slug == profile.TeamSlug) ||
			(profile.TeamRef == "" && profile.TeamSlug == "" && profile.TeamID != "" && team.ID == profile.TeamID) {
			return team, nil
		}
	}
	if len(teams) == 0 {
		return api.Team{}, errors.New("no teams are available")
	}
	return teams[0], nil
}
