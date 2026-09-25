package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/InfrastryAI/infra/internal/auth"
	"github.com/InfrastryAI/infra/internal/config"
	"github.com/spf13/cobra"
)

const skipInitialization = "infrastry.dev/skip-initialization"

var statusCompletionValues = []string{
	"draft", "queued", "analyzing", "planning", "deploying", "healthy", "degraded", "failed", "cancelled",
}

type deployOptions struct {
	name, branch, remote string
	requestID            string
	json, detach, yes    bool
}

type appsOptions struct {
	status string
	json   bool
}

type logsOptions struct {
	tail                   int
	follow                 bool
	poll                   time.Duration
	since, component, data string
	json, timestamps       bool
}

func (command *CLI) rootCommand() (*cobra.Command, error) {
	defaultPath := command.dependencies.Getenv("INFRASTRY_CONFIG")
	if defaultPath == "" {
		var err error
		defaultPath, err = config.DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	defaultURL := command.dependencies.Getenv("INFRASTRY_API_URL")
	if defaultURL == "" {
		defaultURL = config.DefaultAPIURL
	}
	command.configPath = defaultPath
	command.apiURLFlag = defaultURL

	root := &cobra.Command{
		Use:           "infra",
		Short:         "Infrastry from the terminal",
		Long:          "Manage Infrastry authentication, teams, applications, deployments, and logs from the terminal.",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          usageNoArgs("infra does not accept positional arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd == cmd.Root() || skipsInitialization(cmd) {
				return nil
			}
			return command.initialize(cmd)
		},
	}
	root.SetIn(command.dependencies.Stdin)
	root.SetOut(command.dependencies.Stdout)
	root.SetErr(command.dependencies.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err.Error()} })
	root.PersistentFlags().StringVar(&command.apiURLFlag, "api-url", command.apiURLFlag, "Infrastry installation URL")
	root.PersistentFlags().StringVar(&command.configPath, "config", command.configPath, "configuration file path")
	root.PersistentFlags().BoolVar(&command.noColor, "no-color", false, "disable colored output")
	root.PersistentFlags().BoolVar(&command.noInput, "no-input", false, "disable interactive prompts")
	_ = root.MarkPersistentFlagFilename("config", "json")

	version := defaultString(command.dependencies.Version.Version, "dev")
	root.Version = version
	root.SetVersionTemplate(fmt.Sprintf("infra %s (commit %s, built %s)\n",
		version,
		defaultString(command.dependencies.Version.Commit, "unknown"),
		defaultString(command.dependencies.Version.Date, "unknown"),
	))

	root.AddGroup(
		&cobra.Group{ID: "account", Title: "Account and Context:"},
		&cobra.Group{ID: "operations", Title: "Application Operations:"},
		&cobra.Group{ID: "other", Title: "Other Commands:"},
	)
	authCommand := command.authCommand()
	authCommand.GroupID = "account"
	configCommand := command.configCommand()
	configCommand.GroupID = "account"
	teamsCommand := command.teamsCommand()
	teamsCommand.GroupID = "account"
	appsCommand := command.appsCommand()
	appsCommand.GroupID = "operations"
	deployCommand := command.deployCommand()
	deployCommand.GroupID = "operations"
	logsCommand := command.logsCommand()
	logsCommand.GroupID = "operations"
	completionCommand := command.completionCommand()
	completionCommand.GroupID = "other"
	versionCommand := command.versionCommand()
	versionCommand.GroupID = "other"
	root.AddCommand(authCommand, configCommand, teamsCommand, appsCommand, deployCommand, logsCommand, completionCommand, versionCommand)
	command.networkCommands(root)
	return root, nil
}

func (command *CLI) authCommand() *cobra.Command {
	authCommand := &cobra.Command{
		Use:   "auth",
		Short: "Manage browser-based authentication",
		Long:  "Authenticate with OAuth authorization code + PKCE. Tokens are stored in the operating system keyring when available.",
		Args:  usageNoArgs("auth does not accept positional arguments"),
		RunE:  helpCommand,
	}

	var noBrowser bool
	var scopes []string
	var super bool
	var timeout time.Duration
	login := &cobra.Command{
		Use:     "login",
		Short:   "Sign in through your browser",
		Long:    "Sign in through your browser. Use --super for all CLI tools (recommended), or --scope for selected capabilities: " + strings.Join(auth.ScopeNames(), ", ") + ". Each option includes app discovery and a renewable login. Database access includes network access. Team permissions still apply.",
		Example: "  infra auth login --super\n  infra auth login --scope network\n  infra auth login --scope database,logs",
		Args:    usageNoArgs("auth login does not accept positional arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !super && len(scopes) == 0 {
				return usageError{"use infra auth login --super (recommended) or --scope with a comma-separated list of capabilities"}
			}
			return command.runAuthLogin(cmd.Context(), noBrowser, timeout, scopes, super)
		},
	}
	login.Flags().BoolVar(&noBrowser, "no-browser", false, "print the authorization URL instead of opening it")
	login.Flags().StringSliceVar(&scopes, "scope", nil, "enable selected capabilities: "+strings.Join(auth.ScopeNames(), ", "))
	login.Flags().BoolVar(&super, "super", false, "enable all CLI tools (recommended)")
	login.MarkFlagsMutuallyExclusive("scope", "super")
	login.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "time to wait for browser authorization")

	var jsonOutput bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the active authentication",
		Args:  usageNoArgs("auth status does not accept positional arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return command.runAuthStatus(jsonOutput)
		},
	}
	status.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")

	logout := &cobra.Command{
		Use:   "logout",
		Short: "Remove locally stored credentials",
		Args:  usageNoArgs("auth logout does not accept positional arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			return command.runAuthLogout()
		},
	}
	authCommand.AddCommand(login, status, logout)
	return authCommand
}

func (command *CLI) configCommand() *cobra.Command {
	configCommand := &cobra.Command{
		Use:   "config",
		Short: "View or change CLI configuration",
		Args:  usageNoArgs("config does not accept positional arguments"),
		RunE:  helpCommand,
	}
	get := &cobra.Command{
		Use:       "get <api|team>",
		Short:     "Show a configuration value",
		Args:      usageExactArgs(1, "config get requires exactly one key"),
		ValidArgs: []string{"api", "team"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return command.runConfigGet(cmd.Context(), args[0])
		},
	}
	set := &cobra.Command{
		Use:   "set <api|team> [value]",
		Short: "Change a configuration value",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageError{"config set requires a key and value"}
			}
			return nil
		},
		ValidArgsFunction: command.completeConfigSet,
		RunE: func(cmd *cobra.Command, args []string) error {
			value := ""
			if len(args) == 2 {
				value = args[1]
			}
			return command.runConfigSet(cmd.Context(), args[0], value)
		},
	}
	configCommand.AddCommand(get, set)
	return configCommand
}

func (command *CLI) teamsCommand() *cobra.Command {
	teamsCommand := &cobra.Command{
		Use:   "teams",
		Short: "Manage team context",
		Args:  usageNoArgs("teams does not accept positional arguments"),
		RunE:  helpCommand,
	}
	var jsonOutput bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List your available teams",
		Args:  usageNoArgs("teams list does not accept positional arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return command.runTeamsList(cmd.Context(), jsonOutput)
		},
	}
	list.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	teamsCommand.AddCommand(list)
	return teamsCommand
}

func (command *CLI) appsCommand() *cobra.Command {
	appsCommand := &cobra.Command{
		Use:     "apps",
		Aliases: []string{"app"},
		Short:   "List applications and running containers in the selected team",
		Args:    usageNoArgs("apps list does not accept positional arguments"),
	}
	parentOptions := appsOptions{}
	command.bindAppsFlags(appsCommand, &parentOptions)
	appsCommand.RunE = func(cmd *cobra.Command, _ []string) error {
		return command.executeApps(cmd.Context(), parentOptions)
	}

	listOptions := appsOptions{}
	list := &cobra.Command{
		Use:   "list",
		Short: "List applications and running containers in the selected team",
		Args:  usageNoArgs("apps list does not accept positional arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return command.executeApps(cmd.Context(), listOptions)
		},
	}
	command.bindAppsFlags(list, &listOptions)
	appsCommand.AddCommand(list)
	return appsCommand
}

func (command *CLI) bindAppsFlags(cmd *cobra.Command, options *appsOptions) {
	cmd.Flags().StringVar(&options.status, "status", "", "only show applications with this status")
	cmd.Flags().BoolVar(&options.json, "json", false, "emit JSON")
	_ = cmd.RegisterFlagCompletionFunc("status", fixedCompletions(statusCompletionValues))
}

func (command *CLI) executeApps(ctx context.Context, options appsOptions) error {
	team, err := command.currentTeam(ctx, !options.json)
	if err != nil {
		return err
	}
	return command.runApps(ctx, team, options)
}

func (command *CLI) deployCommand() *cobra.Command {
	options := deployOptions{remote: "origin"}
	deploy := &cobra.Command{
		Use:   "deploy [directory]",
		Short: "Deploy an application from the current directory",
		Long: "Review and confirm deployments that create a new application. Deployments to existing applications proceed without confirmation. " +
			"Deploy an application folder, initializing Git and creating an initial commit when needed. " +
			"Local source is uploaded to Infrastry, and later deploys reuse the linked application. Existing repositories must have a clean working tree. " +
			"Public GitHub repositories can also deploy a commit pushed to the selected branch. " +
			"Follow stages and agent activity until completion. Ctrl-C stops watching; deployment continues on Infrastry. " +
			"Infrastry Git uploads the committed source to the linked application using your existing login.",
		Args: usageMaximumArgs(1, "deploy accepts at most one application directory"),
		RunE: func(cmd *cobra.Command, args []string) error {
			team, err := command.currentTeam(cmd.Context(), !options.json)
			if err != nil {
				return err
			}
			return command.runDeploy(cmd.Context(), args, team, options)
		},
	}
	deploy.Flags().StringVar(&options.name, "name", "", "application name (defaults to the directory name)")
	deploy.Flags().StringVar(&options.branch, "branch", "", "branch to deploy (defaults to the checked-out branch)")
	deploy.Flags().StringVar(&options.remote, "remote", "origin", "Git remote to inspect for a public repository")
	deploy.Flags().BoolVar(&options.json, "json", false, "emit newline-delimited JSON events")
	deploy.Flags().BoolVar(&options.detach, "detach", false, "return after acceptance; deployment continues on Infrastry")
	deploy.Flags().BoolVarP(&options.yes, "yes", "y", false, "approve a new application deployment without prompting (required when non-interactive)")
	deploy.Flags().StringVar(&options.requestID, "request-id", "", "reuse a previous submission ID to safely retry it")
	deploy.AddCommand(command.deployWatchCommand())
	return deploy
}

func (command *CLI) logsCommand() *cobra.Command {
	logsCommand := &cobra.Command{
		Use:               "logs [application]",
		Aliases:           []string{"log"},
		Short:             "Tail an application's logs",
		Long:              "Tail logs by application slug, full team/application ref, or exact name. Omit the application in an interactive terminal to choose one.",
		Args:              usageMaximumArgs(1, "logs accepts at most one application"),
		ValidArgsFunction: command.completeApps,
	}
	parentOptions := defaultLogsOptions()
	command.bindLogsFlags(logsCommand, &parentOptions)
	logsCommand.RunE = func(cmd *cobra.Command, args []string) error {
		return command.executeLogs(cmd.Context(), args, parentOptions)
	}

	tailOptions := defaultLogsOptions()
	tail := &cobra.Command{
		Use:               "tail [application]",
		Short:             "Tail an application's logs",
		Args:              usageMaximumArgs(1, "logs tail accepts at most one application"),
		ValidArgsFunction: command.completeApps,
		RunE: func(cmd *cobra.Command, args []string) error {
			return command.executeLogs(cmd.Context(), args, tailOptions)
		},
	}
	command.bindLogsFlags(tail, &tailOptions)
	logsCommand.AddCommand(tail)
	return logsCommand
}

func defaultLogsOptions() logsOptions {
	return logsOptions{tail: 100, follow: true, poll: time.Second, timestamps: true}
}

func (command *CLI) bindLogsFlags(cmd *cobra.Command, options *logsOptions) {
	cmd.Flags().IntVar(&options.tail, "tail", 100, "number of existing events to show (0-1000)")
	cmd.Flags().BoolVar(&options.follow, "follow", true, "continue waiting for new events")
	cmd.Flags().DurationVar(&options.poll, "poll", time.Second, "follow polling interval")
	cmd.Flags().StringVar(&options.since, "since", "", "show events since a duration or RFC3339 time")
	cmd.Flags().StringVar(&options.component, "component", "", "only show a component ID")
	cmd.Flags().StringVar(&options.data, "dataset", "", "only show platform, build, runtime, or access events")
	cmd.Flags().BoolVar(&options.json, "json", false, "emit newline-delimited JSON")
	cmd.Flags().BoolVar(&options.timestamps, "timestamps", true, "show event timestamps in human output")
	_ = cmd.RegisterFlagCompletionFunc("dataset", fixedCompletions([]string{"platform", "build", "runtime", "access"}))
}

func (command *CLI) executeLogs(ctx context.Context, arguments []string, options logsOptions) error {
	team, err := command.currentTeam(ctx, !options.json)
	if err != nil {
		return err
	}
	return command.runLogs(ctx, arguments, team, options)
}

func (command *CLI) completionCommand() *cobra.Command {
	completion := &cobra.Command{
		Use:         "completion <bash|zsh|fish|powershell>",
		Short:       "Generate a shell completion script",
		Args:        usageExactArgs(1, "completion requires exactly one shell"),
		ValidArgs:   []string{"bash", "zsh", "fish", "powershell"},
		Annotations: map[string]string{skipInitialization: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch strings.ToLower(args[0]) {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			default:
				return usageError{fmt.Sprintf("unsupported shell %q", args[0])}
			}
		},
	}
	return completion
}

func (command *CLI) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "version",
		Short:       "Print version information",
		Args:        usageNoArgs("version does not accept positional arguments"),
		Annotations: map[string]string{skipInitialization: "true"},
		Run: func(_ *cobra.Command, _ []string) {
			command.printVersion()
		},
	}
}

func helpCommand(cmd *cobra.Command, _ []string) error { return cmd.Help() }

func skipsInitialization(cmd *cobra.Command) bool {
	for current := cmd; current != nil; current = current.Parent() {
		if current.Annotations[skipInitialization] == "true" || current.Name() == "help" {
			return true
		}
	}
	return false
}

func usageNoArgs(message string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return usageError{message}
		}
		return nil
	}
}

func usageExactArgs(count int, message string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != count {
			return usageError{message}
		}
		return nil
	}
}

func usageMaximumArgs(count int, message string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > count {
			return usageError{message}
		}
		return nil
	}
}

func fixedCompletions(values []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

func cobraUsageError(err error) bool {
	message := err.Error()
	return strings.HasPrefix(message, "unknown command ") ||
		strings.HasPrefix(message, "unknown flag:") ||
		strings.HasPrefix(message, "unknown shorthand flag:")
}
