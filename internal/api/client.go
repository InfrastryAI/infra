package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type TokenSource interface {
	Token(context.Context) (string, error)
	Refresh(context.Context) (string, error)
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Tokens     TokenSource
	UserAgent  string
}

// TransportError identifies a request that failed before the API returned an
// HTTP response. Callers that continuously poll may safely reconnect after
// this kind of interruption while preserving their last acknowledged cursor.
type TransportError struct {
	Err error
}

func (failure *TransportError) Error() string {
	return fmt.Sprintf("contact Infrastry: %v", failure.Err)
}

func (failure *TransportError) Unwrap() error {
	return failure.Err
}

type Team struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

type listTeamsResponse struct {
	Teams []Team `json:"teams"`
}

func (client *Client) ListTeams(ctx context.Context) ([]Team, error) {
	var response listTeamsResponse
	if err := client.get(ctx, "/api/v1/teams", nil, &response); err != nil {
		return nil, err
	}
	if response.Teams == nil {
		response.Teams = []Team{}
	}
	for index, team := range response.Teams {
		if team.ID == "" || team.Slug == "" || team.Ref == "" || team.Name == "" {
			return nil, fmt.Errorf("decode Infrastry response: team at index %d is missing an ID, slug, ref, or name", index)
		}
	}
	return response.Teams, nil
}

type App struct {
	ID              string      `json:"id"`
	Slug            string      `json:"slug"`
	Ref             string      `json:"ref"`
	Name            string      `json:"name"`
	Status          string      `json:"status"`
	Phase           string      `json:"phase"`
	Region          string      `json:"region"`
	RepositoryURL   string      `json:"repository_url"`
	Branch          string      `json:"branch"`
	LiveURL         string      `json:"live_url,omitempty"`
	Team            Team        `json:"team"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Containers      []Container `json:"containers"`
	ContainersError string      `json:"containers_error,omitempty"`
}

// Container describes a running container returned by the application's provider.
type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	Runtime string `json:"runtime"`
}

// HostedRepository is a private Git repository managed by Infrastry. The push
// authorization is short-lived and must never be persisted in local Git
// configuration.
type HostedRepository struct {
	ID                string `json:"id"`
	RepositoryURL     string `json:"repository_url"`
	PushURL           string `json:"push_url"`
	PushAuthorization string `json:"push_authorization"`
}

type CreateHostedRepositoryOptions struct {
	TeamID        string `json:"team_id"`
	Name          string `json:"name"`
	RepositoryURL string `json:"repository_url,omitempty"`
}

type createHostedRepositoryResponse struct {
	Repository HostedRepository `json:"repository"`
}

func (client *Client) CreateHostedRepository(ctx context.Context, options CreateHostedRepositoryOptions) (HostedRepository, error) {
	if options.TeamID == "" {
		return HostedRepository{}, errors.New("team context is required to create a hosted repository")
	}
	if options.Name == "" {
		return HostedRepository{}, errors.New("a name is required to create a hosted repository")
	}

	var response createHostedRepositoryResponse
	if err := client.post(ctx, "/api/v1/repositories", options, &response); err != nil {
		return HostedRepository{}, err
	}
	repository := response.Repository
	if repository.ID == "" || repository.RepositoryURL == "" || repository.PushURL == "" || repository.PushAuthorization == "" {
		return HostedRepository{}, errors.New("decode Infrastry response: hosted repository is missing an ID, repository URL, push URL, or push authorization")
	}
	return repository, nil
}

type DeployOptions struct {
	IdempotencyKey string `json:"idempotency_key"`
	TeamID         string `json:"team_id"`
	AppID          string `json:"app_id,omitempty"`
	Name           string `json:"name"`
	RepositoryURL  string `json:"repository_url"`
	Branch         string `json:"branch"`
	Revision       string `json:"revision"`
	Region         string `json:"region,omitempty"`
}

const ManagedSourceURL = "infrastry://local"

type Deployment struct {
	ID     string `json:"id,omitempty"`
	Number int    `json:"number,omitempty"`
	Status string `json:"status"`
}

type DeployResult struct {
	GoalID     string      `json:"goal_id"`
	App        App         `json:"app"`
	Deployment *Deployment `json:"deployment,omitempty"`
	Status     string      `json:"status"`
}

func (client *Client) Deploy(ctx context.Context, options DeployOptions) (DeployResult, error) {
	if options.TeamID == "" {
		return DeployResult{}, errors.New("team context is required to deploy an application")
	}
	if options.Name == "" || options.RepositoryURL == "" || options.Branch == "" || options.Revision == "" {
		return DeployResult{}, errors.New("name, repository URL, branch, and revision are required to deploy an application")
	}
	if options.IdempotencyKey == "" {
		return DeployResult{}, errors.New("a submission ID is required to deploy an application safely")
	}

	path := "/api/v1/deployments"
	if options.RepositoryURL == ManagedSourceURL {
		if options.AppID == "" {
			return DeployResult{}, errors.New("an existing application is required to deploy Infrastry Git source")
		}
		path = "/api/v1/managed-deployments"
	}
	var result DeployResult
	if err := client.post(ctx, path, options, &result); err != nil {
		return DeployResult{}, err
	}
	if result.GoalID == "" || result.App.ID == "" || result.App.Slug == "" || result.App.Ref == "" || result.App.Name == "" || result.Status == "" {
		return DeployResult{}, errors.New("decode Infrastry response: deployment is missing application identity, name, or status")
	}
	return result, nil
}

type ListAppsOptions struct {
	TeamSlug          string
	Status            string
	IncludeContainers bool
}

type listAppsResponse struct {
	Apps []App `json:"apps"`
}

func (client *Client) ListApps(ctx context.Context, options ListAppsOptions) ([]App, error) {
	if options.TeamSlug == "" {
		return nil, errors.New("team context is required to list applications")
	}
	query := url.Values{}
	query.Set("team", options.TeamSlug)
	if options.IncludeContainers {
		query.Set("include", "containers")
	}
	if options.Status != "" {
		query.Set("status", options.Status)
	}
	var response listAppsResponse
	if err := client.get(ctx, "/api/v1/apps", query, &response); err != nil {
		return nil, err
	}
	if response.Apps == nil {
		response.Apps = []App{}
	}
	for index, app := range response.Apps {
		if app.ID == "" || app.Slug == "" || app.Ref == "" || app.Name == "" ||
			app.Team.ID == "" || app.Team.Slug == "" || app.Team.Ref == "" || app.Team.Name == "" {
			return nil, fmt.Errorf("decode Infrastry response: application at index %d is missing app or team identity", index)
		}
	}
	return response.Apps, nil
}

type LogEvent struct {
	ID            string         `json:"id"`
	Cursor        string         `json:"cursor,omitempty"`
	AppID         string         `json:"app_id"`
	DeploymentID  string         `json:"deployment_id,omitempty"`
	BuildID       string         `json:"build_id,omitempty"`
	Level         string         `json:"level"`
	Source        string         `json:"source"`
	Dataset       string         `json:"dataset"`
	ComponentID   string         `json:"component_id,omitempty"`
	ComponentName string         `json:"component_name,omitempty"`
	ComponentType string         `json:"component_type,omitempty"`
	Message       string         `json:"message"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	OccurredAt    time.Time      `json:"occurred_at"`
	ObservedAt    time.Time      `json:"observed_at"`
}

type ListLogsOptions struct {
	Tail        int
	Limit       int
	Cursor      string
	Since       string
	ComponentID string
	Dataset     string
}

type LogPage struct {
	App        App        `json:"app"`
	Logs       []LogEvent `json:"logs"`
	NextCursor string     `json:"next_cursor"`
}

func (client *Client) ListLogs(ctx context.Context, teamSlug, appSlug string, options ListLogsOptions) (LogPage, error) {
	if teamSlug == "" {
		return LogPage{}, errors.New("team context is required to list logs")
	}
	if appSlug == "" {
		return LogPage{}, errors.New("application slug is required to list logs")
	}
	query := url.Values{}
	if options.Cursor != "" {
		query.Set("cursor", options.Cursor)
		query.Set("limit", strconv.Itoa(options.Limit))
	} else {
		query.Set("tail", strconv.Itoa(options.Tail))
		if options.Since != "" {
			query.Set("since", options.Since)
		}
	}
	if options.ComponentID != "" {
		query.Set("component_id", options.ComponentID)
	}
	if options.Dataset != "" {
		query.Set("dataset", options.Dataset)
	}

	var response LogPage
	path := "/api/v1/teams/" + url.PathEscape(teamSlug) + "/apps/" + url.PathEscape(appSlug) + "/logs"
	if err := client.get(ctx, path, query, &response); err != nil {
		return response, err
	}
	if response.Logs == nil {
		response.Logs = []LogEvent{}
	}
	return response, nil
}

func (client *Client) get(ctx context.Context, path string, query url.Values, target any) error {
	return client.request(ctx, http.MethodGet, path, query, nil, target)
}

func (client *Client) post(ctx context.Context, path string, payload, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Infrastry request: %w", err)
	}
	return client.request(ctx, http.MethodPost, path, nil, body, target)
}

func (client *Client) request(ctx context.Context, method, path string, query url.Values, body []byte, target any) error {
	if client.HTTPClient == nil {
		client.HTTPClient = http.DefaultClient
	}
	if client.Tokens == nil {
		return errors.New("API token source is not configured")
	}

	token, err := client.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	response, err := client.do(ctx, method, path, query, body, token)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		token, err = client.Tokens.Refresh(ctx)
		if err != nil {
			return err
		}
		response, err = client.do(ctx, method, path, query, body, token)
		if err != nil {
			return err
		}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeHTTPError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode Infrastry response: %w", err)
	}
	return nil
}

func (client *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, token string) (*http.Response, error) {
	endpoint := strings.TrimRight(client.BaseURL, "/") + path
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	var requestBody io.Reader
	if body != nil {
		requestBody = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return nil, fmt.Errorf("build Infrastry request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if client.UserAgent != "" {
		request.Header.Set("User-Agent", client.UserAgent)
	}
	// Bearer tokens must stay on the selected API origin. In particular, the
	// default redirect policy can forward Authorization to the same host after
	// an HTTPS-to-HTTP downgrade.
	privateHTTP := *client.HTTPClient
	privateHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := privateHTTP.Do(request)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	return response, nil
}

type HTTPError struct {
	StatusCode int
	Message    string
	RequestID  string
}

func (failure *HTTPError) Error() string {
	message := failure.Message
	if message == "" {
		message = http.StatusText(failure.StatusCode)
	}
	if failure.RequestID != "" {
		return fmt.Sprintf("Infrastry returned %d: %s (request %s)", failure.StatusCode, message, failure.RequestID)
	}
	return fmt.Sprintf("Infrastry returned %d: %s", failure.StatusCode, message)
}

func decodeHTTPError(response *http.Response) error {
	failure := &HTTPError{
		StatusCode: response.StatusCode,
		RequestID:  response.Header.Get("X-Request-ID"),
	}
	var body struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err == nil {
		failure.Message = body.Message
		if failure.Message == "" {
			switch value := body.Error.(type) {
			case string:
				failure.Message = value
			case map[string]any:
				if message, ok := value["message"].(string); ok {
					failure.Message = message
				}
			}
		}
	}
	return failure
}
