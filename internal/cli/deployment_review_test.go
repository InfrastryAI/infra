package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/config"
	"github.com/InfrastryAI/infra/internal/source"
)

func TestNewAppDeployRequiresApprovalBeforeAnyMutation(t *testing.T) {
	for _, sourceKind := range []string{"fresh", "unlinked", "public-new"} {
		for _, scenario := range []struct {
			name, input string
			flags       []string
			terminal    bool
			wantCode    int
		}{
			{name: "cancel", input: "c\n", terminal: true},
			{name: "no", input: "n\n", terminal: true},
			{name: "eof", terminal: true},
			{name: "partial-answer", input: "yes", terminal: true},
			{name: "invalid-answer", input: "maybe\nc\n", terminal: true},
			{name: "edit-eof", input: "e\n", terminal: true},
			{name: "edit-cancel", terminal: true},
			{name: "non-interactive", input: "yes\n", wantCode: 2},
			{name: "no-input", flags: []string{"--no-input"}, input: "yes\n", terminal: true, wantCode: 2},
			{name: "json", flags: []string{"--json"}, wantCode: 2},
			{name: "detach", flags: []string{"--detach"}, wantCode: 2},
		} {
			t.Run(sourceKind+"/"+scenario.name, func(t *testing.T) {
				if scenario.name == "edit-cancel" {
					scenario.input = "e\n" + strings.Repeat("\n", 4) + "c\n"
				}
				control := &fakeSourceControl{repository: source.Repository{Root: "/work/app", Branch: "main", Revision: strings.Repeat("a", 40)}}
				if sourceKind == "fresh" {
					control.repository = source.Repository{}
					control.inspectError = source.ErrNotRepository
				}
				if strings.HasPrefix(sourceKind, "public-") {
					control.public, control.publicURL = true, "https://github.com/acme/app.git"
				}
				mutations := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						mutations++
						t.Errorf("mutated before approval: %s %s", r.Method, r.URL)
					}
					if r.Method == "GET" && r.URL.Path == "/api/v1/apps" {
						apps := []api.App{}
						json.NewEncoder(w).Encode(map[string]any{"apps": apps})
						return
					}
					http.NotFound(w, r)
				}))
				defer server.Close()
				configPath := deployConfiguration(t, server.URL)
				store := config.Store{Path: configPath}
				settings, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				profile, _ := settings.Profile(server.URL)
				profile.Scopes = append(profile.Scopes, "apps:deploy")
				settings.SetProfile(profile)
				if err := store.Save(settings); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				command := New(Dependencies{Stdin: strings.NewReader(scenario.input), Stdout: &stdout, Stderr: &stderr, Source: control, HTTPClient: server.Client(), Credentials: &config.MemoryCredentialStore{}, Getenv: func(string) string { return "" }, WorkingDir: func() (string, error) { return "/work/app", nil }, IsTerminal: func() bool { return scenario.terminal }})
				args := append([]string{"--api-url", server.URL, "--config", configPath, "deploy"}, scenario.flags...)
				code := command.Run(t.Context(), args)
				if code != scenario.wantCode {
					t.Fatalf("exit %d, expected %d: %s", code, scenario.wantCode, stderr.String())
				}
				if mutations != 0 || control.initialized || control.pushed || control.ensuredRemote != "" {
					t.Fatalf("changed source or application without approval: %+v, mutations=%d", control, mutations)
				}
				for _, expected := range []string{"Review deployment", "Directory: /work/app", "Team: acme", "Branch: main"} {
					if !strings.Contains(stderr.String(), expected) {
						t.Errorf("missing review detail %q: %s", expected, stderr.String())
					}
				}
				if scenario.wantCode == 2 && !strings.Contains(stderr.String(), "--yes") {
					t.Fatalf("missing non-interactive approval instructions: %s", stderr.String())
				}
				if scenario.name == "edit-cancel" && strings.Count(stderr.String(), "Review deployment") != 2 {
					t.Fatalf("edited settings were not reviewed before cancellation: %s", stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("review polluted stdout: %s", stdout.String())
				}
			})
		}
	}
}

type forbiddenDeploymentInput struct{ t *testing.T }

func (input forbiddenDeploymentInput) Read([]byte) (int, error) {
	input.t.Error("existing application deployment read confirmation input")
	return 0, errors.New("unexpected confirmation prompt")
}

func TestExistingAppDeployDoesNotRequireApproval(t *testing.T) {
	for _, sourceKind := range []string{"managed-linked", "public-matched"} {
		for _, mode := range []string{"interactive", "non-interactive", "no-input"} {
			t.Run(sourceKind+"/"+mode, func(t *testing.T) {
				managed := strings.HasPrefix(sourceKind, "managed-")
				team := api.Team{ID: "team-1", Slug: "acme", Ref: "acme", Name: "Acme"}
				app := api.App{ID: "app-1", Slug: "app", Ref: "acme/app", Name: "Existing App", Team: team, Branch: "main", RepositoryURL: "https://github.com/acme/app.git"}
				control := &fakeSourceControl{repository: source.Repository{Root: "/work/app", Branch: "main", Revision: strings.Repeat("a", 40)}}
				endpoint := "/api/v1/deployments"
				if managed {
					app.RepositoryURL = api.ManagedSourceURL
					endpoint = "/api/v1/managed-deployments"
				} else {
					control.public, control.publicURL = true, app.RepositoryURL
				}
				submitted := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == "GET" && r.URL.Path == "/api/v1/apps":
						json.NewEncoder(w).Encode(map[string]any{"apps": []api.App{app}})
					case r.Method == "POST" && r.URL.Path == endpoint:
						submitted++
						var request api.DeployOptions
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.AppID != app.ID || request.Revision != control.repository.Revision {
							t.Errorf("incorrect redeployment: %+v, %v", request, err)
						}
						json.NewEncoder(w).Encode(api.DeployResult{App: app, GoalID: "goal-1", Status: "queued"})
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				if sourceKind == "managed-linked" {
					control.repository.HostedRepositoryURL = server.URL + "/git/acme/app.git"
				}
				configPath := deployConfiguration(t, server.URL)
				store := config.Store{Path: configPath}
				settings, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				profile, _ := settings.Profile(server.URL)
				profile.Scopes = append(profile.Scopes, "apps:deploy")
				settings.SetProfile(profile)
				if err := store.Save(settings); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				command := New(Dependencies{Stdin: forbiddenDeploymentInput{t}, Stdout: &stdout, Stderr: &stderr, Source: control, HTTPClient: server.Client(), Credentials: &config.MemoryCredentialStore{}, Getenv: func(string) string { return "" }, WorkingDir: func() (string, error) { return "/work/app", nil }, IsTerminal: func() bool { return mode != "non-interactive" }})
				args := []string{"--api-url", server.URL, "--config", configPath, "deploy", "--json", "--detach"}
				if mode == "no-input" {
					args = append(args, "--no-input")
				}
				if code := command.Run(t.Context(), args); code != 0 || submitted != 1 || control.pushed != managed {
					t.Fatalf("redeployment failed: code=%d submitted=%d pushed=%t: %s", code, submitted, control.pushed, stderr.String())
				}
				if strings.Contains(stderr.String(), "Review deployment") || strings.Contains(stderr.String(), "edit settings, or cancel") {
					t.Fatalf("existing application requested confirmation: %s", stderr.String())
				}
				if !json.Valid(stdout.Bytes()) {
					t.Fatalf("invalid deployment output: %s", stdout.String())
				}
			})
		}
	}
}

func TestDeploymentReviewInterruptsBlockedInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	var output bytes.Buffer
	command := New(Dependencies{Stdin: reader, Stderr: &output, IsTerminal: func() bool { return true }, Source: &fakeSourceControl{inspectError: source.ErrNotRepository}})
	command.apiURL = "https://infrastry.test"
	command.configuration.SetProfile(config.Profile{APIURL: command.apiURL, Scopes: []string{"apps:deploy"}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := command.reviewDeployment(ctx, nil, "/work/app", "/work/app", api.Team{Ref: "acme"}, deployOptions{})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupt returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deployment confirmation did not stop on interruption")
	}
}
