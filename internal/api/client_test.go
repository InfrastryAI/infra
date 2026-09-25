package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeTokens struct {
	token     string
	refreshed string
	refreshes int
}

func (tokens *fakeTokens) Token(context.Context) (string, error) { return tokens.token, nil }
func (tokens *fakeTokens) Refresh(context.Context) (string, error) {
	tokens.refreshes++
	return tokens.refreshed, nil
}

func TestListTeams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/teams" {
			t.Errorf("path = %q", request.URL.Path)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"teams": []map[string]any{{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"}},
		})
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: &fakeTokens{token: "token"}}
	teams, err := client.ListTeams(context.Background())
	if err != nil {
		t.Fatalf("ListTeams() error = %v", err)
	}
	if len(teams) != 1 || teams[0].ID != "team-1" || teams[0].Slug != "acme" || teams[0].Ref != "acme" || teams[0].Name != "Acme" {
		t.Fatalf("ListTeams() = %#v", teams)
	}
}

func TestAuthenticatedRequestsDoNotFollowRedirects(t *testing.T) {
	redirected := 0
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirected++
		if request.Header.Get("Authorization") != "" {
			t.Error("bearer token reached redirect destination")
		}
	}))
	defer destination.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", destination.URL+"/stolen")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := Client{BaseURL: origin.URL, HTTPClient: origin.Client(), Tokens: &fakeTokens{token: "access-secret"}}
	if _, err := client.ListTeams(t.Context()); err == nil {
		t.Fatal("accepted redirected API response")
	}
	if redirected != 0 {
		t.Fatalf("followed authenticated redirect %d times", redirected)
	}
}

func TestListAppsRetriesUnauthorizedAfterRefresh(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer fresh" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.URL.Query().Get("team") != "acme" {
			t.Errorf("team = %q", request.URL.Query().Get("team"))
		}
		if _, present := request.URL.Query()["team_id"]; present {
			t.Errorf("obsolete team_id query was sent: %v", request.URL.Query())
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"apps": []map[string]any{{
				"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Customer API",
				"team":       map[string]any{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
				"updated_at": "2026-08-17T12:00:00Z",
			}},
		})
	}))
	defer server.Close()

	tokens := &fakeTokens{token: "stale", refreshed: "fresh"}
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: tokens}
	apps, err := client.ListApps(context.Background(), ListAppsOptions{TeamSlug: "acme"})
	if err != nil {
		t.Fatalf("ListApps() error = %v", err)
	}
	if len(apps) != 1 || apps[0].ID != "app-1" || apps[0].Slug != "customer-api" || apps[0].Ref != "acme/customer-api" {
		t.Fatalf("ListApps() = %#v", apps)
	}
	if requests != 2 || tokens.refreshes != 1 {
		t.Fatalf("requests/refreshes = %d/%d", requests, tokens.refreshes)
	}
}

func TestListLogsBuildsCursorQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/teams/acme/apps/customer-api/logs" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("cursor") != "opaque cursor" || request.URL.Query().Get("limit") != "1000" {
			t.Errorf("query = %v", request.URL.Query())
		}
		if _, present := request.URL.Query()["team_id"]; present {
			t.Errorf("obsolete team_id query was sent: %v", request.URL.Query())
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"app": map[string]any{
				"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Customer API",
				"team": map[string]any{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
			},
			"logs": []any{}, "next_cursor": "next",
		})
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: &fakeTokens{token: "token"}}
	page, err := client.ListLogs(context.Background(), "acme", "customer-api", ListLogsOptions{Cursor: "opaque cursor", Limit: 1000})
	if err != nil {
		t.Fatalf("ListLogs() error = %v", err)
	}
	if page.NextCursor != "next" || page.App.ID != "app-1" || page.App.Ref != "acme/customer-api" {
		t.Fatalf("ListLogs() = %#v", page)
	}
}

func TestApplicationRequestsRequireTeamContext(t *testing.T) {
	client := Client{}
	if _, err := client.ListApps(context.Background(), ListAppsOptions{}); err == nil {
		t.Fatal("ListApps() error = nil")
	}
	if _, err := client.ListLogs(context.Background(), "", "customer-api", ListLogsOptions{}); err == nil {
		t.Fatal("ListLogs() error = nil")
	}
	if _, err := client.ListLogs(context.Background(), "acme", "", ListLogsOptions{}); err == nil {
		t.Fatal("ListLogs() missing app slug error = nil")
	}
	if _, err := client.CreateHostedRepository(context.Background(), CreateHostedRepositoryOptions{}); err == nil {
		t.Fatal("CreateHostedRepository() error = nil")
	}
	if _, err := client.Deploy(context.Background(), DeployOptions{}); err == nil {
		t.Fatal("Deploy() error = nil")
	}
}

func TestCreateHostedRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/repositories" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", request.Header.Get("Content-Type"))
		}
		var body CreateHostedRepositoryOptions
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		if body.TeamID != "team-1" || body.Name != "Customer API" || body.RepositoryURL != "https://git.example/old.git" {
			t.Errorf("body = %#v", body)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"repository": map[string]any{
			"id": "repo-1", "repository_url": "https://git.example/repo-1.git",
			"push_url": "https://git.example/repo-1.git", "push_authorization": "Bearer temporary",
		}})
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: &fakeTokens{token: "token"}}
	repository, err := client.CreateHostedRepository(t.Context(), CreateHostedRepositoryOptions{
		TeamID: "team-1", Name: "Customer API", RepositoryURL: "https://git.example/old.git",
	})
	if err != nil {
		t.Fatalf("CreateHostedRepository() error = %v", err)
	}
	if repository.ID != "repo-1" || repository.PushAuthorization != "Bearer temporary" {
		t.Fatalf("CreateHostedRepository() = %#v", repository)
	}
}

func TestDeployRetriesPostBodyAfterRefresh(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		var options DeployOptions
		if err := json.Unmarshal(body, &options); err != nil {
			t.Fatalf("decode body %q: %v", body, err)
		}
		if options.Revision != "0123456789abcdef" || options.AppID != "app-1" {
			t.Errorf("body = %#v", options)
		}
		if request.Header.Get("Authorization") != "Bearer fresh" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"app": map[string]any{
				"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Customer API",
			},
			"status": "queued", "goal_id": "goal-1",
		})
	}))
	defer server.Close()

	tokens := &fakeTokens{token: "stale", refreshed: "fresh"}
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: tokens}
	result, err := client.Deploy(t.Context(), DeployOptions{
		IdempotencyKey: "request-123456",
		TeamID:         "team-1", AppID: "app-1", Name: "Customer API",
		RepositoryURL: "https://code.example/customer.git", Branch: "main", Revision: "0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if result.Status != "queued" || result.App.ID != "app-1" || result.App.Ref != "acme/customer-api" {
		t.Fatalf("Deploy() = %#v", result)
	}
	if requests != 2 || tokens.refreshes != 1 {
		t.Fatalf("requests/refreshes = %d/%d", requests, tokens.refreshes)
	}
}

func TestListAppsIncludesContainers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("include") != "containers" || request.URL.Query().Get("status") != "healthy" {
			t.Errorf("query = %v", request.URL.Query())
		}
		io.WriteString(writer, `{"apps":[{"id":"app-1","slug":"api","ref":"acme/api","name":"API","team":{"id":"team-1","slug":"acme","ref":"acme","name":"Acme"},"containers":[{"id":"web-1","name":"web","image":"web:latest","status":"Running","runtime":"Node.js"}]}]}`)
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: &fakeTokens{token: "token"}}
	apps, err := client.ListApps(t.Context(), ListAppsOptions{TeamSlug: "acme", Status: "healthy", IncludeContainers: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || len(apps[0].Containers) != 1 || apps[0].Containers[0].Image != "web:latest" || apps[0].Containers[0].Runtime != "Node.js" {
		t.Fatalf("apps = %#v", apps)
	}
	encoded, err := json.Marshal(apps)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded[0]["containers"].([]any)) != 1 {
		t.Fatalf("JSON = %s", encoded)
	}
}
