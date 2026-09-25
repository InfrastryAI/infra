package api

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

type ManagedApp struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Branch       string `json:"branch"`
	Ref          string `json:"ref"`
	GitRemoteURL string `json:"git_remote_url"`
}

type CreateManagedAppOptions struct {
	Name           string `json:"name"`
	Branch         string `json:"branch"`
	Region         string `json:"region,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (client *Client) CreateManagedApp(ctx context.Context, team string, options CreateManagedAppOptions) (ManagedApp, error) {
	if team == "" || options.Name == "" || options.Branch == "" || options.IdempotencyKey == "" {
		return ManagedApp{}, errors.New("team, name, branch, and submission ID are required to create an application")
	}
	var response struct {
		App ManagedApp `json:"app"`
	}
	if err := client.post(ctx, "/api/v1/teams/"+url.PathEscape(team)+"/git-apps", options, &response); err != nil {
		return ManagedApp{}, err
	}
	app := response.App
	parts := strings.Split(app.Ref, "/")
	if app.ID == "" || app.Name == "" || app.Branch != options.Branch || app.GitRemoteURL == "" || len(parts) != 2 || parts[0] != team || parts[1] == "" {
		return ManagedApp{}, errors.New("decode Infrastry response: application is missing its identity, branch, or Git remote")
	}
	return app, nil
}
