package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/source"
)

func (command *CLI) runManagedDeploy(ctx context.Context, client *api.Client, repository source.Repository, team api.Team, options deployOptions, app api.App) error {
	if err := command.requireSourceUploadPermission(); err != nil {
		return err
	}
	requestID, err := deploymentRequestID(options.requestID)
	if err != nil {
		return err
	}
	options.requestID = requestID
	if app.ID == "" {
		created, err := client.CreateManagedApp(ctx, team.Slug, api.CreateManagedAppOptions{Name: options.name, Branch: repository.Branch, IdempotencyKey: requestID})
		if err != nil {
			return err
		}
		createdRef, err := managedRemoteRef(command.apiURL, created.GitRemoteURL)
		if err != nil || createdRef != created.Ref {
			return errors.New("the application's Git remote does not match this installation; check its settings before uploading source")
		}
		fmt.Fprintf(command.dependencies.Stderr, "Using application %s.\n", terminalText(created.Ref))
		app = api.App{ID: created.ID, Name: created.Name, Ref: created.Ref, Slug: strings.TrimPrefix(created.Ref, team.Slug+"/"), Branch: created.Branch, RepositoryURL: api.ManagedSourceURL, Team: team}
	}
	if app.Branch != repository.Branch {
		return fmt.Errorf("application %s deploys branch %s; use --branch %s to deploy this commit", terminalText(app.Ref), terminalText(app.Branch), terminalText(app.Branch))
	}
	// Build the push destination from authenticated application identity. Never
	// send the API token to a URL supplied only by local Git configuration.
	pushURL := strings.TrimRight(command.apiURL, "/") + "/git/" + url.PathEscape(team.Slug) + "/" + url.PathEscape(app.Slug) + ".git"
	if repository.HostedRepositoryURL == "" {
		if err := command.dependencies.Source.EnsureRemote(ctx, repository, "infrastry", pushURL); err != nil {
			return fmt.Errorf("link application %s: %w; copy its Git remote from the application's settings before retrying", terminalText(app.Ref), err)
		}
	}
	err = command.withSpinner(ctx, "Uploading source to Infrastry…", !options.json, func(uploadContext context.Context) error {
		token, err := client.Tokens.Token(uploadContext)
		if err != nil {
			return err
		}
		return command.dependencies.Source.Push(uploadContext, repository, pushURL, "Bearer "+token)
	})
	if err != nil {
		return err
	}
	return command.submitDeployment(ctx, client, repository, options, api.DeployOptions{
		TeamID: team.ID, AppID: app.ID, Name: app.Name,
		RepositoryURL: api.ManagedSourceURL, Branch: repository.Branch,
		Revision: repository.Revision,
	}, "Infrastry Git · "+app.Ref)
}

func (command *CLI) requireSourceUploadPermission() error {
	profile, _ := command.configuration.Profile(command.apiURL)
	if !slices.Contains(profile.Scopes, "apps:deploy") {
		return errors.New("your login needs permission to upload and deploy source; sign in again with infra auth login --super or --scope deploy")
	}
	return nil
}

func (command *CLI) managedDeploymentApp(ctx context.Context, client *api.Client, repository source.Repository, team api.Team) (api.App, error) {
	ref := ""
	if repository.HostedRepositoryURL != "" {
		var err error
		ref, err = managedRemoteRef(command.apiURL, repository.HostedRepositoryURL)
		if err != nil {
			return api.App{}, err
		}
		if !strings.HasPrefix(ref, team.Slug+"/") {
			return api.App{}, errors.New("the infrastry Git remote belongs to another team; select its team with infra config set team before deploying")
		}
	}
	if ref == "" {
		// Application creation is deferred until the deployment is approved.
		return api.App{}, nil
	}
	apps, err := client.ListApps(ctx, api.ListAppsOptions{TeamSlug: team.Slug})
	if err != nil {
		return api.App{}, err
	}
	app, err := command.resolveApp(ctx, apps, ref)
	if err != nil {
		return api.App{}, err
	}
	if app.Ref != ref || app.RepositoryURL != api.ManagedSourceURL {
		return api.App{}, errors.New("the application does not match the infrastry Git remote; check git remote -v before deploying")
	}
	return app, nil
}

func managedRemoteRef(apiURL, remote string) (string, error) {
	base, baseErr := url.Parse(apiURL)
	parsed, err := url.Parse(remote)
	if baseErr != nil || err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!strings.EqualFold(parsed.Scheme, base.Scheme) || !strings.EqualFold(parsed.Host, base.Host) {
		return "", errors.New("the infrastry Git remote does not match this API address; use --api-url for the installation that owns the application")
	}
	prefix := strings.TrimRight(base.Path, "/") + "/git/"
	if !strings.HasPrefix(parsed.Path, prefix) {
		return "", errors.New("the infrastry Git remote is not an application push URL; copy the Git remote from the application's settings")
	}
	ref := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(parsed.Path, prefix), "/"), ".git")
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", errors.New("the infrastry Git remote is not an application push URL; copy the Git remote from the application's settings")
	}
	return ref, nil
}
