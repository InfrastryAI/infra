package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/config"
	"github.com/InfrastryAI/infra/internal/source"
)

type managedSourceControl struct {
	fakeSourceControl
	pushURL, authorization string
	pushError              error
}

func (control *managedSourceControl) Push(_ context.Context, _ source.Repository, target, authorization string) error {
	control.pushed, control.pushURL, control.authorization = true, target, authorization
	return control.pushError
}

func TestDeployExistingInfrastryRemote(t *testing.T) {
	for _, scenario := range []string{"follow", "detach", "source-mismatch", "branch-mismatch", "other-installation", "other-team", "missing-scope", "push-failure", "invalid-request-id"} {
		t.Run(scenario, func(t *testing.T) {
			team := api.Team{ID: "team-1", Slug: "acme", Ref: "acme", Name: "Acme"}
			app := api.App{ID: "app-1", Slug: "gin", Ref: "acme/gin", Name: "Sample Gin", Team: team, Branch: "main", RepositoryURL: api.ManagedSourceURL}
			other := api.App{ID: "app-2", Slug: "other", Ref: "acme/other", Name: "Sample Gin", Team: team, Branch: "main", RepositoryURL: api.ManagedSourceURL}
			control := &managedSourceControl{fakeSourceControl: fakeSourceControl{repository: source.Repository{Root: "/work/gin", Branch: "main", Revision: strings.Repeat("a", 40)}}}
			posts, reads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/api/v1/apps":
					json.NewEncoder(w).Encode(map[string]any{"apps": []api.App{other, app}})
				case r.Method == "POST" && r.URL.Path == "/api/v1/managed-deployments":
					posts++
					var request api.DeployOptions
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					if !control.pushed || request.AppID != app.ID || request.RepositoryURL != api.ManagedSourceURL || request.Revision != control.repository.Revision || request.Name != app.Name || request.IdempotencyKey == "" {
						t.Errorf("incorrect managed submission: %+v", request)
					}
					json.NewEncoder(w).Encode(api.DeployResult{App: app, GoalID: "goal-1", Status: "queued"})
				case r.Method == "GET" && r.URL.Path == "/api/v1/deployments/goal-1":
					http.Error(w, "goals:read is not granted", http.StatusForbidden)
				case r.Method == "GET" && r.URL.Path == "/api/v1/managed-deployments/goal-1":
					reads++
					json.NewEncoder(w).Encode(api.DeploymentPage{Goal: api.DeploymentGoal{ID: "goal-1", Status: "succeeded", Stage: "complete"}, Events: []api.DeploymentEvent{{ID: "event-1", Type: "stage", Title: "Build image", Status: "succeeded"}}, NextCursor: "c1"})
				default:
					t.Errorf("unexpected API operation: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			control.repository.HostedRepositoryURL = server.URL + "/git/acme/gin.git"
			configPath := deployConfiguration(t, server.URL)
			store := config.Store{Path: configPath}
			configuration, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			profile, _ := configuration.Profile(server.URL)
			if scenario != "missing-scope" {
				profile.Scopes = append(profile.Scopes, "apps:deploy")
			}
			configuration.SetProfile(profile)
			if err := store.Save(configuration); err != nil {
				t.Fatal(err)
			}
			args := []string{"--api-url", server.URL, "--config", configPath, "deploy", "--json"}
			switch scenario {
			case "detach":
				args = append(args, "--detach")
			case "source-mismatch":
				app.RepositoryURL = "https://github.com/acme/app.git"
			case "branch-mismatch":
				control.repository.Branch = "feature"
			case "other-installation":
				control.repository.HostedRepositoryURL = "https://elsewhere.test/git/acme/gin.git"
			case "other-team":
				control.repository.HostedRepositoryURL = server.URL + "/git/other/gin.git"
			case "push-failure":
				control.pushError = errors.New("source upload failed")
			case "invalid-request-id":
				args = append(args, "--request-id", "short")
			}
			var stdout, stderr bytes.Buffer
			command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, Source: control, HTTPClient: server.Client(), Credentials: &config.MemoryCredentialStore{}, Getenv: func(string) string { return "" }, WorkingDir: func() (string, error) { return "/work/gin", nil }})
			code := command.Run(t.Context(), args)
			if scenario == "follow" || scenario == "detach" {
				if code != 0 || posts != 1 || !control.pushed || control.authorization != "Bearer access" || control.pushURL != server.URL+"/git/acme/gin.git" {
					t.Fatalf("exit=%d posts=%d pushed=%t: %s", code, posts, control.pushed, stderr.String())
				}
				wantReads, wantLines := 1, 3
				if scenario == "detach" {
					wantReads, wantLines = 0, 1
				}
				lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
				if reads != wantReads || len(lines) != wantLines {
					t.Fatalf("reads=%d output=%s", reads, stdout.String())
				}
				for _, line := range lines {
					if !json.Valid([]byte(line)) {
						t.Errorf("non-JSON output: %s", line)
					}
				}
			} else if code == 0 || posts != 0 || (scenario != "push-failure" && control.pushed) {
				t.Fatalf("invalid source was submitted: exit=%d posts=%d pushed=%t: %s", code, posts, control.pushed, stderr.String())
			}
			if control.initialized || control.ensuredRemote != "" {
				t.Fatal("deploy changed the checkout or remote")
			}
		})
	}
}

func TestManagedRemoteRefRejectsUntrustedDestinations(t *testing.T) {
	for _, remote := range []string{
		"https://evil.test/git/acme/gin.git", "http://infrastry.test/git/acme/gin.git",
		"https://user:secret@infrastry.test/git/acme/gin.git", "https://infrastry.test/git/acme/gin.git?token=secret",
		"https://infrastry.test/git/acme/gin.git#fragment", "https://infrastry.test/other/acme/gin.git",
		"https://infrastry.test/git/acme/../gin.git", "https://infrastry.test/git/acme/.git", "https://infrastry.test/git/../gin.git",
	} {
		if _, err := managedRemoteRef("https://infrastry.test", remote); err == nil {
			t.Errorf("accepted untrusted remote %s", remote)
		}
	}
	if ref, err := managedRemoteRef("http://localhost:4000", "http://localhost:4000/git/acme/gin.git"); err != nil || ref != "acme/gin" {
		t.Fatalf("valid remote rejected: %s %v", ref, err)
	}
}

func TestManagedAppCreationRetriesWithSameSubmissionID(t *testing.T) {
	team := api.Team{ID: "team-1", Slug: "acme", Ref: "acme", Name: "Acme"}
	app := api.App{ID: "app-1", Slug: "gin", Ref: "acme/gin", Name: "Sample Gin", Team: team, Branch: "main", RepositoryURL: api.ManagedSourceURL}
	control := &managedSourceControl{fakeSourceControl: fakeSourceControl{repository: source.Repository{Root: "/work/gin", Branch: "main", Revision: strings.Repeat("a", 40)}}}
	created, attempts, submitted := 0, 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps":
			json.NewEncoder(w).Encode(map[string]any{"apps": []api.App{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/teams/acme/git-apps":
			attempts++
			var request api.CreateManagedAppOptions
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.IdempotencyKey != "retry-create-123" || request.Name != app.Name || request.Branch != app.Branch {
				t.Errorf("incorrect creation request: %+v: %v", request, err)
			}
			if attempts == 1 {
				created++
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				connection.Close() // Creation committed; its response was lost.
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"app": api.ManagedApp{ID: app.ID, Ref: app.Ref, Name: app.Name, Branch: app.Branch, GitRemoteURL: server.URL + "/git/acme/gin.git"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/managed-deployments":
			submitted++
			var request api.DeployOptions
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.IdempotencyKey != "retry-create-123" || request.AppID != app.ID {
				t.Errorf("incorrect deployment request: %+v: %v", request, err)
			}
			json.NewEncoder(w).Encode(api.DeployResult{App: app, GoalID: "goal-1", Status: "queued"})
		default:
			t.Errorf("unexpected API operation: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := deployConfiguration(t, server.URL)
	store := config.Store{Path: configPath}
	configuration, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := configuration.Profile(server.URL)
	profile.Scopes = append(profile.Scopes, "apps:deploy")
	configuration.SetProfile(profile)
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}
	credentials := &config.MemoryCredentialStore{}
	for attempt := range 2 {
		var stdout, stderr bytes.Buffer
		command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, Source: control, HTTPClient: server.Client(), Credentials: credentials, Getenv: func(string) string { return "" }, WorkingDir: func() (string, error) { return "/work/gin", nil }})
		code := command.Run(t.Context(), []string{"--api-url", server.URL, "--config", configPath, "deploy", "--name", "Sample Gin", "--yes", "--detach", "--request-id", "retry-create-123"})
		if !strings.Contains(stderr.String(), "Submission: retry-create-123") {
			t.Fatalf("attempt %d did not print the submission ID before app creation: %s", attempt, stderr.String())
		}
		if attempt == 0 && (code == 0 || control.pushed || control.ensuredRemote != "") {
			t.Fatalf("lost response unexpectedly continued: code=%d stderr=%s", code, stderr.String())
		}
		if attempt == 1 && code != 0 {
			t.Fatalf("retry failed: %s", stderr.String())
		}
	}
	if created != 1 || attempts != 2 || submitted != 1 || !control.pushed {
		t.Fatalf("created=%d attempts=%d submitted=%d pushed=%t", created, attempts, submitted, control.pushed)
	}
}
