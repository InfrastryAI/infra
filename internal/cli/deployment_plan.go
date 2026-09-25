package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/source"
)

// Planning may inspect Git and query applications, but must not change the
// checkout, create an application, upload source, or submit a deployment.
type deploymentPlan struct {
	repository    source.Repository
	repositoryURL string
	app           api.App
	initialCommit bool
	initializeGit bool
	managed       bool
}

func (command *CLI) planDeployment(ctx context.Context, client *api.Client, directory string, team api.Team, options deployOptions) (deploymentPlan, error) {
	var plan deploymentPlan
	repository, err := command.dependencies.Source.Inspect(ctx, directory, options.remote, options.branch)
	plan.initializeGit = errors.Is(err, source.ErrNotRepository)
	plan.initialCommit = plan.initializeGit || errors.Is(err, source.ErrNoCommits)
	if err != nil && !plan.initialCommit {
		return plan, err
	}
	if repository.Root == "" {
		repository.Root = directory
	}
	if plan.initialCommit {
		repository.Branch = defaultString(repository.Branch, defaultString(strings.TrimSpace(options.branch), "main"))
	} else if repository.Dirty {
		return plan, errors.New("the Git working tree has uncommitted or untracked files; commit or ignore them before deploying")
	}
	plan.repository = repository

	if repository.HostedRepositoryURL != "" || (plan.initialCommit && repository.RemoteURL == "") {
		plan.managed = true
	} else {
		var public bool
		plan.repositoryURL, public = command.dependencies.Source.PublicRemote(ctx, repository)
		plan.managed = !public && repository.RemoteURL == ""
		if !plan.managed && (!public || !isGitHubRepositoryURL(plan.repositoryURL) || plan.initialCommit) {
			return plan, errors.New("without an infrastry Git remote, deployment supports only public GitHub repositories with the current commit pushed to the selected branch")
		}
	}
	if plan.managed {
		if err := command.requireSourceUploadPermission(); err != nil {
			return plan, err
		}
		plan.app, err = command.managedDeploymentApp(ctx, client, repository, team)
		if err != nil {
			return plan, err
		}
		if plan.app.ID != "" && plan.app.Branch != repository.Branch {
			return plan, fmt.Errorf("application %s deploys branch %s; use --branch %s to deploy this commit", terminalText(plan.app.Ref), terminalText(plan.app.Branch), terminalText(plan.app.Branch))
		}
		return plan, nil
	}

	apps, err := client.ListApps(ctx, api.ListAppsOptions{TeamSlug: team.Slug})
	if err != nil {
		return plan, err
	}
	appID, err := matchingAppID(apps, plan.repositoryURL, repository.Branch)
	if err != nil {
		return plan, err
	}
	for _, app := range apps {
		if app.ID == appID {
			plan.app = app
			break
		}
	}
	return plan, nil
}

func deploymentName(plan deploymentPlan, requested string) (string, error) {
	name := strings.TrimSpace(requested)
	if plan.app.ID != "" {
		name = plan.app.Name
	} else if name == "" {
		name = applicationNameFromDirectory(filepath.Clean(plan.repository.Root))
	}
	if utf8.RuneCountInString(name) < 2 || utf8.RuneCountInString(name) > 80 {
		return "", usageError{"application name must contain between 2 and 80 characters"}
	}
	return name, nil
}

func (command *CLI) printDeploymentPlan(plan deploymentPlan, team api.Team, options deployOptions) {
	out := command.dependencies.Stderr
	fmt.Fprintf(out, "\nWelcome to Infrastry\nReview deployment\n  Directory: %s\n  Team: %s\n", terminalText(plan.repository.Root), terminalText(team.Ref))
	if plan.app.ID == "" {
		fmt.Fprintf(out, "  Application: %s (create new)\n", terminalText(options.name))
	} else {
		fmt.Fprintf(out, "  Application: %s (%s, existing)\n", terminalText(plan.app.Name), terminalText(plan.app.Ref))
	}
	fmt.Fprintf(out, "  Branch: %s\n", terminalText(plan.repository.Branch))
	if !plan.initialCommit {
		fmt.Fprintf(out, "  Commit: %s\n", terminalText(plan.repository.Revision))
	}
	if plan.initializeGit {
		fmt.Fprintln(out, "  • Initialize a Git repository in this directory.")
	}
	if plan.initialCommit {
		fmt.Fprintln(out, "  • Stage files and create an initial commit, honoring .gitignore.")
	}
	if plan.app.ID == "" {
		fmt.Fprintln(out, "  • Create a new application in the selected team.")
	}
	if plan.managed {
		if plan.repository.HostedRepositoryURL == "" {
			fmt.Fprintln(out, "  • Add an infrastry Git remote linking this checkout to the application.")
		}
		fmt.Fprintln(out, "  • Upload committed source to Infrastry and start deployment.")
	} else {
		fmt.Fprintf(out, "  • Deploy source from %s.\n", terminalText(plan.repositoryURL))
	}
}
