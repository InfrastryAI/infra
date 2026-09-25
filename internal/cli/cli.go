package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/auth"
	"github.com/InfrastryAI/infra/internal/config"
	"github.com/InfrastryAI/infra/internal/source"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/term"
)

type Version struct {
	Version string
	Commit  string
	Date    string
}

type Dependencies struct {
	NetworkKey  func(apiURL, team, mode string) (private, public string, err error)
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	HTTPClient  *http.Client
	Credentials config.CredentialStore
	OpenURL     func(string) error
	Getenv      func(string) string
	Now         func() time.Time
	WorkingDir  func() (string, error)
	IsTerminal  func() bool
	Source      source.Control
	Version     Version
}

type CLI struct {
	dependencies      Dependencies
	colors            bool
	apiURL            string
	apiURLFlag        string
	configPath        string
	noColor           bool
	noInput           bool
	initialized       bool
	credentialSources map[string]string
	credentialErrors  map[string]error
	store             config.Store
	configuration     config.Config
}

type usageError struct{ message string }

func (failure usageError) Error() string { return failure.message }

func New(dependencies Dependencies) *CLI {
	if dependencies.Stdin == nil {
		dependencies.Stdin = os.Stdin
	}
	if dependencies.Stdout == nil {
		dependencies.Stdout = os.Stdout
	}
	if dependencies.Stderr == nil {
		dependencies.Stderr = os.Stderr
	}
	if dependencies.HTTPClient == nil {
		dependencies.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if dependencies.Credentials == nil {
		dependencies.Credentials = config.KeyringCredentialStore{}
	}
	if dependencies.OpenURL == nil {
		dependencies.OpenURL = auth.OpenBrowser
	}
	if dependencies.Getenv == nil {
		dependencies.Getenv = os.Getenv
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if dependencies.WorkingDir == nil {
		dependencies.WorkingDir = os.Getwd
	}
	if dependencies.Source == nil {
		dependencies.Source = source.Git{}
	}
	if dependencies.IsTerminal == nil {
		dependencies.IsTerminal = func() bool {
			return inputIsTerminal(dependencies.Stdin) && outputIsTerminal(dependencies.Stderr)
		}
	}
	return &CLI{
		dependencies:      dependencies,
		credentialSources: make(map[string]string),
		credentialErrors:  make(map[string]error),
	}
}

func (command *CLI) Run(ctx context.Context, arguments []string) int {
	root, err := command.rootCommand()
	if err == nil {
		root.SetArgs(arguments)
		err = root.ExecuteContext(ctx)
	}
	if err != nil {
		var usage usageError
		if errors.As(err, &usage) || cobraUsageError(err) {
			message := usage.message
			if message == "" {
				message = err.Error()
			}
			fmt.Fprintf(command.dependencies.Stderr, "Error: %s\n", message)
			return 2
		}
		if errors.Is(err, context.Canceled) {
			return 0
		}
		fmt.Fprintf(command.dependencies.Stderr, "Error: %s\n", err)
		return 1
	}
	return 0
}

func (command *CLI) runDeploy(ctx context.Context, arguments []string, team api.Team, options deployOptions) error {
	workingDirectory, err := command.dependencies.WorkingDir()
	if err != nil {
		return fmt.Errorf("find current directory: %w", err)
	}
	directory := workingDirectory
	if len(arguments) == 1 {
		directory = arguments[0]
	}
	client, err := command.apiClient()
	if err != nil {
		return err
	}

	plan, options, err := command.reviewDeployment(ctx, client, workingDirectory, directory, team, options)
	if errors.Is(err, errDeploymentCancelled) {
		fmt.Fprintln(command.dependencies.Stderr, "Deployment cancelled. No source or application changes were made.")
		return nil
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintf(command.dependencies.Stderr, "Submission: %s (reuse with --request-id if interrupted before acceptance).\n", options.requestID)
	repository := plan.repository
	if plan.initialCommit {
		err = command.withSpinner(ctx, "Preparing application source…", !options.json, func(prepareContext context.Context) error {
			var prepareErr error
			repository, prepareErr = command.dependencies.Source.Initialize(prepareContext, repository.Root, repository.Branch, options.remote)
			return prepareErr
		})
		if err != nil {
			return err
		}
	}
	if plan.managed {
		return command.runManagedDeploy(ctx, client, repository, team, options, plan.app)
	}
	return command.submitDeployment(ctx, client, repository, options, api.DeployOptions{
		TeamID: team.ID, AppID: plan.app.ID, Name: options.name,
		RepositoryURL: plan.repositoryURL, Branch: repository.Branch,
		Revision: repository.Revision,
	}, plan.repositoryURL)
}

func (command *CLI) submitDeployment(ctx context.Context, client *api.Client, repository source.Repository, options deployOptions, request api.DeployOptions, sourceLabel string) error {
	requestID, err := deploymentRequestID(options.requestID)
	if err != nil {
		return err
	}
	var result api.DeployResult
	request.IdempotencyKey = requestID
	err = command.withSpinner(ctx, "Queueing deployment…", !options.json, func(actionContext context.Context) error {
		var deployErr error
		result, deployErr = client.Deploy(actionContext, request)
		return deployErr
	})
	if err != nil {
		return err
	}
	watchOptions := deploymentWatchOptions{
		json: options.json, follow: true, poll: time.Second,
		name:   result.App.Name,
		source: result.App.Ref + " · " + repository.Branch + " · " + shortRevision(repository.Revision),
	}
	if options.json {
		if err := writeDeploymentJSON(command.dependencies.Stdout, map[string]any{"type": "accepted", "deployment": result}); err != nil {
			return err
		}
	} else if options.detach || !command.deploymentUIEnabled(watchOptions) {
		fmt.Fprintf(command.dependencies.Stdout, "Deployment queued for %s (%s).\n", terminalText(result.App.Name), terminalText(result.App.Ref))
		fmt.Fprintf(command.dependencies.Stdout, "Source: %s#%s (%s)\n", terminalText(sourceLabel), terminalText(repository.Branch), shortRevision(repository.Revision))
	}
	command.printDeploymentResume(result.GoalID)
	if options.detach {
		return nil
	}
	return command.watchDeployment(ctx, client, result.GoalID, watchOptions)
}

func (command *CLI) runConfigGet(ctx context.Context, key string) error {
	switch normalizeConfigKey(key) {
	case "api":
		fmt.Fprintln(command.dependencies.Stdout, command.apiURL)
		return nil
	case "team":
		team, err := command.currentTeam(ctx, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(command.dependencies.Stdout, "%s (%s)\n", team.Ref, team.Name)
		return nil
	default:
		return usageError{fmt.Sprintf("unknown configuration key %q", key)}
	}
}

func (command *CLI) runConfigSet(ctx context.Context, key, value string) error {
	key = normalizeConfigKey(key)
	switch key {
	case "api":
		if value == "" {
			return usageError{"config set api requires a URL"}
		}
		apiURL, err := config.NormalizeAPIURL(value)
		if err != nil {
			return usageError{err.Error()}
		}
		command.configuration.SetCurrentAPIURL(apiURL)
		if err := command.persistConfiguration(false); err != nil {
			return err
		}
		fmt.Fprintf(command.dependencies.Stdout, "Set api to %s.\n", apiURL)
		return nil
	case "team":
		teams, err := command.listTeams(ctx)
		if err != nil {
			return err
		}
		var team api.Team
		if value == "" {
			if !command.canPrompt() {
				return usageError{"config set team requires a team slug, ref, or name when prompts are disabled"}
			}
			team, err = command.selectTeam(ctx, teams, "Select a team")
		} else {
			team, err = command.resolveTeam(ctx, teams, value)
		}
		if err != nil {
			return err
		}
		if err := command.saveTeam(team); err != nil {
			return err
		}
		fmt.Fprintf(command.dependencies.Stdout, "Set team to %s (%s).\n", team.Ref, team.Name)
		return nil
	default:
		return usageError{fmt.Sprintf("unknown configuration key %q", key)}
	}
}

func (command *CLI) runAuthLogin(ctx context.Context, noBrowser bool, timeout time.Duration, scopes []string, super bool) error {
	profile, err := auth.Login(ctx, auth.LoginOptions{
		APIURL:     command.apiURL,
		Scopes:     scopes,
		Super:      super,
		HTTPClient: command.dependencies.HTTPClient,
		OpenURL:    command.dependencies.OpenURL,
		Output:     command.dependencies.Stderr,
		NoBrowser:  noBrowser,
		Timeout:    timeout,
		Now:        command.dependencies.Now,
	})
	if err != nil {
		return err
	}
	command.configuration.SetProfile(profile)
	if err := command.persistConfiguration(true); err != nil {
		return err
	}
	fmt.Fprintf(command.dependencies.Stdout, "Authenticated with %s.\n", command.apiURL)
	return nil
}

func (command *CLI) runAuthStatus(jsonOutput bool) error {
	profile, ok := command.configuration.Profile(command.apiURL)
	if !ok || (profile.AccessToken == "" && profile.RefreshToken == "") {
		return command.authenticationError()
	}
	status := "active"
	if !profile.ExpiresAt.After(command.dependencies.Now()) {
		status = "refreshable"
		if profile.RefreshToken == "" {
			status = "expired"
		}
	}
	if jsonOutput {
		return writeJSON(command.dependencies.Stdout, map[string]any{
			"api_url":    profile.APIURL,
			"status":     status,
			"expires_at": profile.ExpiresAt,
			"scopes":     profile.Scopes,
		})
	}
	fmt.Fprintf(command.dependencies.Stdout, "Authenticated with %s\n", profile.APIURL)
	fmt.Fprintf(command.dependencies.Stdout, "Status: %s\n", status)
	fmt.Fprintf(command.dependencies.Stdout, "Access token expires: %s\n", profile.ExpiresAt.Local().Format(time.RFC1123))
	fmt.Fprintf(command.dependencies.Stdout, "Scopes: %s\n", strings.Join(profile.Scopes, ", "))
	return nil
}

func (command *CLI) runAuthLogout() error {
	if !command.configuration.RemoveProfile(command.apiURL) {
		fmt.Fprintf(command.dependencies.Stdout, "No saved authentication for %s.\n", command.apiURL)
		return nil
	}
	if command.credentialSources[command.apiURL] == "keyring" {
		if err := command.dependencies.Credentials.Delete(command.apiURL); err != nil && !errors.Is(err, config.ErrCredentialsNotFound) {
			return fmt.Errorf("remove credentials from OS keyring: %w", err)
		}
	} else {
		_ = command.dependencies.Credentials.Delete(command.apiURL)
	}
	if err := command.persistConfiguration(false); err != nil {
		return err
	}
	fmt.Fprintf(command.dependencies.Stdout, "Removed local authentication for %s.\n", command.apiURL)
	return nil
}

func (command *CLI) runTeamsList(ctx context.Context, jsonOutput bool) error {
	teams, err := command.listTeams(ctx)
	if err != nil {
		return err
	}
	profile, _ := command.configuration.Profile(command.apiURL)
	if profile.TeamID == "" && profile.TeamSlug == "" && profile.TeamRef == "" && len(teams) > 0 {
		if err := command.saveTeam(teams[0]); err != nil {
			return err
		}
		profile.TeamID = teams[0].ID
		profile.TeamSlug = teams[0].Slug
		profile.TeamRef = teams[0].Ref
	}
	if jsonOutput {
		return writeJSON(command.dependencies.Stdout, teams)
	}
	command.printTeams(teams, profile)
	return nil
}

func (command *CLI) runApps(ctx context.Context, team api.Team, options appsOptions) error {
	if options.status != "" && !validStatus(options.status) {
		return usageError{fmt.Sprintf("invalid status %q", options.status)}
	}

	client, err := command.apiClient()
	if err != nil {
		return err
	}
	apps, err := client.ListApps(ctx, api.ListAppsOptions{TeamSlug: team.Slug, Status: options.status, IncludeContainers: true})
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(command.dependencies.Stdout, apps)
	}
	command.printApps(apps)
	return nil
}

func (command *CLI) runLogs(ctx context.Context, arguments []string, team api.Team, options logsOptions) error {
	if options.tail < 0 || options.tail > 1000 {
		return usageError{"tail must be between 0 and 1000"}
	}
	if options.poll < 200*time.Millisecond {
		return usageError{"poll must be at least 200ms"}
	}
	if options.data != "" && !oneOf(options.data, "platform", "build", "runtime", "access") {
		return usageError{fmt.Sprintf("invalid dataset %q", options.data)}
	}
	sinceValue, err := normalizeSince(options.since, command.dependencies.Now())
	if err != nil {
		return usageError{err.Error()}
	}

	client, err := command.apiClient()
	if err != nil {
		return err
	}
	apps, err := client.ListApps(ctx, api.ListAppsOptions{TeamSlug: team.Slug})
	if err != nil {
		return err
	}
	var app api.App
	if len(arguments) == 0 {
		if options.json || !command.canPrompt() {
			return usageError{"logs requires an application slug, full ref, or name when prompts are disabled"}
		}
		app, err = command.selectApp(ctx, apps, "Select an application")
	} else {
		app, err = command.resolveApp(ctx, apps, arguments[0])
	}
	if err != nil {
		return err
	}
	if options.follow {
		fmt.Fprintf(command.dependencies.Stderr, "Tailing %s (%s). Press Ctrl-C to stop.\n", app.Name, app.Ref)
	}

	listOptions := api.ListLogsOptions{
		Tail:        options.tail,
		Limit:       1000,
		Since:       sinceValue,
		ComponentID: options.component,
		Dataset:     options.data,
	}
	seen := make(map[string]struct{})
	page, err := client.ListLogs(ctx, app.Team.Slug, app.Slug, listOptions)
	if err != nil {
		return err
	}
	for _, event := range page.Logs {
		if _, duplicate := seen[event.ID]; duplicate {
			continue
		}
		seen[event.ID] = struct{}{}
		command.printLog(event, options.json, options.timestamps)
	}
	if !options.follow {
		return nil
	}
	listOptions.Cursor = page.NextCursor
	listOptions.Since = ""
	if listOptions.Cursor == "" {
		return errors.New("the logs API did not return next_cursor, so following cannot resume safely")
	}

	backoff := options.poll
	consecutiveFailures := 0
	interruptionReported := false
	for {
		if err := wait(ctx, backoff); err != nil {
			return nil
		}
		page, err = client.ListLogs(ctx, app.Team.Slug, app.Slug, listOptions)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !retryable(err) {
				return err
			}
			consecutiveFailures++
			if consecutiveFailures == 3 {
				fmt.Fprintln(command.dependencies.Stderr, "Log connection is temporarily unavailable. Retrying…")
				interruptionReported = true
			}
			backoff = minDuration(backoff*2, 30*time.Second)
			continue
		}
		if interruptionReported {
			fmt.Fprintln(command.dependencies.Stderr, "Log connection restored.")
		}
		consecutiveFailures = 0
		interruptionReported = false
		backoff = options.poll
		if page.NextCursor != "" {
			listOptions.Cursor = page.NextCursor
		}
		for _, event := range page.Logs {
			if _, duplicate := seen[event.ID]; duplicate {
				continue
			}
			seen[event.ID] = struct{}{}
			command.printLog(event, options.json, options.timestamps)
		}
		if len(seen) > 5000 {
			seen = recentIDs(page.Logs)
		}
	}
}

func (command *CLI) currentTeam(ctx context.Context, allowPrompt bool) (api.Team, error) {
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
	if len(teams) == 0 {
		return api.Team{}, errors.New("no teams are available for this account; create a team in Infrastry and try again")
	}
	if profile.TeamID != "" || profile.TeamSlug != "" || profile.TeamRef != "" {
		for _, team := range teams {
			matchesRef := profile.TeamRef != "" && team.Ref == profile.TeamRef
			matchesSlug := profile.TeamRef == "" && profile.TeamSlug != "" && team.Slug == profile.TeamSlug
			matchesLegacyID := profile.TeamRef == "" && profile.TeamSlug == "" && team.ID == profile.TeamID
			if matchesRef || matchesSlug || matchesLegacyID {
				if err := command.saveTeam(team); err != nil {
					return api.Team{}, err
				}
				return team, nil
			}
		}
		if allowPrompt && command.canPrompt() {
			team, err := command.selectTeam(ctx, teams, "The selected team is unavailable; choose another")
			if err != nil {
				return api.Team{}, err
			}
			if err := command.saveTeam(team); err != nil {
				return api.Team{}, err
			}
			return team, nil
		}
		return api.Team{}, errors.New("the selected team is no longer available; run `infra config set team <slug>` to select another")
	}
	team := teams[0]
	if allowPrompt && command.canPrompt() && len(teams) > 1 {
		team, err = command.selectTeam(ctx, teams, "Select a default team")
		if err != nil {
			return api.Team{}, err
		}
	}
	if err := command.saveTeam(team); err != nil {
		return api.Team{}, err
	}
	fmt.Fprintf(command.dependencies.Stderr, "Selected default team %s (%s).\n", team.Ref, team.Name)
	return team, nil
}

func (command *CLI) listTeams(ctx context.Context) ([]api.Team, error) {
	client, err := command.apiClient()
	if err != nil {
		return nil, err
	}
	return client.ListTeams(ctx)
}

func (command *CLI) saveTeam(team api.Team) error {
	if team.ID == "" || team.Slug == "" || team.Ref == "" {
		return errors.New("cannot select a team without complete identity fields")
	}
	if !command.configuration.SetTeam(command.apiURL, team.ID, team.Slug, team.Ref, team.Name) {
		return auth.ErrLoginRequired
	}
	return command.persistConfiguration(false)
}

func resolveTeam(teams []api.Team, selector string) (api.Team, error) {
	for _, team := range teams {
		if team.Slug == selector || team.Ref == selector {
			return team, nil
		}
	}
	var matches []api.Team
	for _, team := range teams {
		if strings.EqualFold(team.Name, selector) {
			matches = append(matches, team)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		refs := make([]string, len(matches))
		for index, team := range matches {
			refs[index] = team.Ref
		}
		sort.Strings(refs)
		return api.Team{}, fmt.Errorf("team name %q is ambiguous; use one of these refs: %s", selector, strings.Join(refs, ", "))
	}
	return api.Team{}, fmt.Errorf("team %q was not found", selector)
}

func normalizeConfigKey(key string) string {
	return strings.ToLower(key)
}

func (command *CLI) apiClient() (*api.Client, error) {
	profile, ok := command.configuration.Profile(command.apiURL)
	if !ok || (profile.AccessToken == "" && profile.RefreshToken == "") {
		return nil, command.authenticationError()
	}
	manager := &auth.TokenManager{
		Profile:    &profile,
		HTTPClient: command.dependencies.HTTPClient,
		Now:        command.dependencies.Now,
		Persist: func(updated config.Profile) error {
			command.configuration.SetProfile(updated)
			return command.persistConfiguration(true)
		},
	}
	version := command.dependencies.Version.Version
	if version == "" {
		version = "dev"
	}
	return &api.Client{
		BaseURL:    command.apiURL,
		HTTPClient: command.dependencies.HTTPClient,
		Tokens:     manager,
		UserAgent:  "infrastry-cli/" + version,
	}, nil
}

func resolveApp(apps []api.App, selector string) (api.App, error) {
	for _, app := range apps {
		if app.Slug == selector || app.Ref == selector {
			return app, nil
		}
	}
	var matches []api.App
	for _, app := range apps {
		if strings.EqualFold(app.Name, selector) {
			matches = append(matches, app)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		refs := make([]string, len(matches))
		for index, app := range matches {
			refs[index] = app.Ref
		}
		sort.Strings(refs)
		return api.App{}, fmt.Errorf("application name %q is ambiguous; use one of these refs: %s", selector, strings.Join(refs, ", "))
	}
	return api.App{}, fmt.Errorf("application %q was not found", selector)
}

func matchingAppID(apps []api.App, repositoryURL, branch string) (string, error) {
	target := normalizeRepositoryURL(repositoryURL)
	var matches []api.App
	for _, app := range apps {
		if normalizeRepositoryURL(app.RepositoryURL) == target && app.Branch == branch {
			matches = append(matches, app)
		}
	}
	if len(matches) == 0 {
		return "", nil
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	refs := make([]string, len(matches))
	for index, app := range matches {
		refs[index] = app.Ref
	}
	sort.Strings(refs)
	return "", fmt.Errorf("more than one application uses %s#%s (%s); resolve the duplicate source associations in Infrastry before deploying", repositoryURL, branch, strings.Join(refs, ", "))
}

func normalizeRepositoryURL(value string) string {
	if canonical, ok := source.CanonicalURL(value); ok {
		value = canonical
	}
	value = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(value), "/"), ".git")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return value
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.User = nil
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), ".git")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func isGitHubRepositoryURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && strings.EqualFold(parsed.Hostname(), "github.com")
}

func applicationNameFromDirectory(directory string) string {
	name := strings.TrimSuffix(filepath.Base(filepath.Clean(directory)), ".git")
	name = strings.NewReplacer("-", " ", "_", " ").Replace(name)
	words := strings.Fields(name)
	for index, word := range words {
		runes := []rune(strings.ToLower(word))
		if len(runes) > 0 {
			runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		}
		words[index] = string(runes)
	}
	name = strings.Join(words, " ")
	if utf8.RuneCountInString(name) < 2 {
		return "New application"
	}
	return name
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func (command *CLI) printApps(apps []api.App) {
	if len(apps) == 0 {
		fmt.Fprintln(command.dependencies.Stdout, "No applications found.")
		return
	}
	writer := command.tableWriter()
	writer.AppendHeader(table.Row{
		command.style("SLUG", "1"),
		command.style("NAME / RUNTIME", "1"),
		command.style("STATUS", "1"),
		command.style("REGION", "1"),
		command.style("TEAM", "1"),
		command.style("URL", "1"),
	})
	for _, app := range apps {
		address := app.LiveURL
		if address == "" {
			address = "—"
		}
		writer.AppendRow(table.Row{command.style(app.Slug, "1"), app.Name, command.statusStyle(app.Status), app.Region, app.Team.Slug, address})
		containers := "No running containers"
		if app.ContainersError != "" {
			containers = "Containers unavailable: " + app.ContainersError
		} else if app.Containers == nil {
			containers = "Containers unavailable"
		} else if len(app.Containers) > 0 {
			for index, container := range app.Containers {
				branch := "  ├─ "
				if index == len(app.Containers)-1 {
					branch = "  └─ "
				}
				name := text.Snip(container.Name, 32, "…")
				runtime := defaultString(container.Runtime, "—")
				writer.AppendRow(table.Row{branch + name, runtime, command.statusStyle(container.Status), "", "", ""})
			}
			continue
		}
		writer.AppendRow(table.Row{"  └─ " + containers, "", "", "", "", ""})
	}
	fmt.Fprintln(command.dependencies.Stdout, writer.Render())
}

func (command *CLI) printTeams(teams []api.Team, profile config.Profile) {
	if len(teams) == 0 {
		fmt.Fprintln(command.dependencies.Stdout, "No teams found.")
		return
	}
	writer := command.tableWriter()
	writer.AppendHeader(table.Row{"", command.style("SLUG", "1"), command.style("NAME", "1")})
	for _, team := range teams {
		marker := ""
		if team.Ref == profile.TeamRef || (profile.TeamRef == "" && team.ID == profile.TeamID) {
			marker = command.style("●", "32")
		}
		writer.AppendRow(table.Row{marker, team.Slug, team.Name})
	}
	fmt.Fprintln(command.dependencies.Stdout, writer.Render())
}

func (command *CLI) tableWriter() table.Writer {
	writer := table.NewWriter()
	style := table.StyleDefault
	style.Options = table.OptionsNoBordersAndSeparators
	style.Box.PaddingLeft = ""
	style.Box.PaddingRight = "  "
	style.Format.Header = text.FormatDefault
	writer.SetStyle(style)
	return writer
}

func (command *CLI) printLog(event api.LogEvent, jsonOutput, timestamps bool) {
	if jsonOutput {
		encoded, err := json.Marshal(event)
		if err == nil {
			fmt.Fprintln(command.dependencies.Stdout, string(encoded))
		}
		return
	}
	component := event.ComponentName
	if component == "" {
		component = event.ComponentID
	}
	if component == "" {
		component = "—"
	}
	prefix := ""
	if timestamps {
		prefix = event.OccurredAt.Local().Format("15:04:05.000") + "  "
	}
	level := strings.ToUpper(defaultString(event.Level, "info"))
	level = fmt.Sprintf("%-7s", level)
	level = command.levelStyle(level, event.Level)
	header := fmt.Sprintf("%s%s %-8s %-16s ", prefix, level, event.Dataset, component)
	lines := strings.Split(event.Message, "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	fmt.Fprintln(command.dependencies.Stdout, header+lines[0])
	indent := strings.Repeat(" ", utf8.RuneCountInString(prefix)+7+1+8+1+16+1)
	for _, line := range lines[1:] {
		fmt.Fprintln(command.dependencies.Stdout, indent+line)
	}
}

func (command *CLI) levelStyle(value, level string) string {
	switch strings.ToLower(level) {
	case "error", "fatal":
		return command.style(value, "31")
	case "warning", "warn":
		return command.style(value, "33")
	case "debug", "trace":
		return command.style(value, "36")
	default:
		return command.style(value, "32")
	}
}

func (command *CLI) statusStyle(value string) string {
	switch value {
	case "healthy":
		return command.style(value, "32")
	case "failed", "degraded":
		return command.style(value, "31")
	case "queued", "analyzing", "planning", "deploying":
		return command.style(value, "33")
	default:
		return value
	}
}

func (command *CLI) style(value, code string) string {
	if !command.colors {
		return value
	}
	style := lipgloss.NewStyle()
	switch code {
	case "1":
		style = style.Bold(true)
	case "31":
		style = style.Foreground(lipgloss.Color("1"))
	case "32":
		style = style.Foreground(lipgloss.Color("2"))
	case "33":
		style = style.Foreground(lipgloss.Color("3"))
	case "36":
		style = style.Foreground(lipgloss.Color("6"))
	}
	return style.Render(value)
}

func (command *CLI) link(address string) string {
	if !command.colors {
		return address
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("4")).
		Underline(true).
		Hyperlink(address).
		Render(address)
}

func (command *CLI) printVersion() {
	version := defaultString(command.dependencies.Version.Version, "dev")
	commit := defaultString(command.dependencies.Version.Commit, "unknown")
	date := defaultString(command.dependencies.Version.Date, "unknown")
	fmt.Fprintf(command.dependencies.Stdout, "infra %s (commit %s, built %s)\n", version, commit, date)
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func normalizeSince(value string, now time.Time) (string, error) {
	if value == "" {
		return "", nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return "", errors.New("since duration cannot be negative")
		}
		return now.Add(-duration).UTC().Format(time.RFC3339Nano), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("since must be a duration such as 30m or an RFC3339 timestamp")
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func validStatus(status string) bool {
	return oneOf(status, "draft", "queued", "analyzing", "planning", "deploying", "healthy", "degraded", "failed", "cancelled")
}

func oneOf(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func retryable(err error) bool {
	var response *api.HTTPError
	if errors.As(err, &response) {
		return response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	}
	var transport *api.TransportError
	if errors.As(err, &transport) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func recentIDs(events []api.LogEvent) map[string]struct{} {
	ids := make(map[string]struct{}, len(events))
	for _, event := range events {
		ids[event.ID] = struct{}{}
	}
	return ids
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func outputIsTerminal(output io.Writer) bool {
	file, ok := output.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func inputIsTerminal(input io.Reader) bool {
	file, ok := input.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
