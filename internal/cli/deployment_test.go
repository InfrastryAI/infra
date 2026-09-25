package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/config"
	"github.com/InfrastryAI/infra/internal/source"
)

func TestDeploymentWatchReplaysStagesAndAgentsAndDrainsTerminalPages(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/deployments/goal-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		calls++
		page := api.DeploymentPage{Goal: api.DeploymentGoal{ID: "goal-1", Status: "succeeded", Stage: "complete", Message: "App is live", Result: api.DeploymentOutcome{URL: "https://app.example"}}}
		switch calls {
		case 1:
			if r.URL.Query().Get("cursor") != "" {
				t.Error("unexpected initial cursor")
			}
			page.Events = []api.DeploymentEvent{{ID: "s1", Type: "stage", Stage: "analysis", Title: "Review source", Status: "running"}, {ID: "a1", Type: "agent", Stage: "analysis", Title: "Reading files", Message: "Reading mix.exs"}}
			page.NextCursor = "c1"
			page.HasMore = true
		case 2:
			if r.URL.Query().Get("cursor") != "c1" {
				t.Error("lost cursor")
			}
			// A temporary failure must not advance the acknowledged cursor.
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case 3:
			if r.URL.Query().Get("cursor") != "c1" {
				t.Error("lost cursor after reconnect")
			}
			page.Events = []api.DeploymentEvent{{ID: "a1", Type: "agent", Title: "Reading files"}, {ID: "s2", Type: "stage", Stage: "verify", Title: "Health checks", Status: "succeeded"}}
			page.NextCursor = "c2"
		default:
			t.Errorf("too many progress calls: %d", calls)
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Credentials: &config.MemoryCredentialStore{}, Getenv: func(string) string { return "" }})
	code := command.Run(t.Context(), []string{"--api-url", server.URL, "--config", deployConfiguration(t, server.URL), "deploy", "watch", "goal-1", "--poll", "100ms"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	output := stdout.String()
	for _, expected := range []string{"Review source", "Reading mix.exs", "Health checks", "https://app.example"} {
		if !strings.Contains(output, expected) {
			t.Errorf("missing %q: %s", expected, output)
		}
	}
	if strings.Count(output, "Reading files") != 1 {
		t.Fatalf("duplicate activity: %s", output)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestDeploymentWatchDetachOnlyReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Errorf("watch mutated deployment: %s", r.Method)
		}
		json.NewEncoder(w).Encode(api.DeploymentPage{Goal: api.DeploymentGoal{ID: "goal-1", Status: "running", Stage: "building"}})
		cancel()
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Credentials: &config.MemoryCredentialStore{}, Getenv: func(string) string { return "" }})
	code := command.Run(ctx, []string{"--api-url", server.URL, "--config", deployConfiguration(t, server.URL), "deploy", "watch", "goal-1"})
	if code != 0 || calls != 1 || !strings.Contains(stderr.String(), "Deployment continues") {
		t.Fatalf("exit %d, calls %d: %s", code, calls, stderr.String())
	}
}

func TestDeploymentWatchTerminalOutcomesAndJSON(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "cancelled", "needs_input", "billing", "unauthorized"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == "unauthorized" {
					http.Error(w, "denied", http.StatusForbidden)
					return
				}
				goal := api.DeploymentGoal{ID: "goal-1", Status: status, Stage: status, Message: "Current outcome"}
				if status == "billing" {
					goal.Status = "running"
					goal.Action = &api.DeploymentAction{Type: "billing_setup", Label: "Add payment method", Path: "/teams/acme/billing"}
				}
				json.NewEncoder(w).Encode(api.DeploymentPage{Goal: goal})
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Credentials: &config.MemoryCredentialStore{}, Getenv: func(string) string { return "" }})
			code := command.Run(t.Context(), []string{"--api-url", server.URL, "--config", deployConfiguration(t, server.URL), "deploy", "watch", "goal-1", "--json"})
			expected := 1
			if status == "succeeded" {
				expected = 0
			}
			if code != expected {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if status != "unauthorized" {
				var event map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &event); err != nil || event["type"] != "status" {
					t.Fatalf("invalid JSON: %s (%v)", stdout.String(), err)
				}
			}
		})
	}
}

func TestDeploymentOutputStripsTerminalControls(t *testing.T) {
	value := terminalText("read\x1b[2J\nfile\u202e")
	if strings.ContainsAny(value, "\x1b\n\u202e") {
		t.Fatalf("unsafe output: %q", value)
	}
}

func TestDeployFollowsByDefaultAndJSONIsOneObjectPerLine(t *testing.T) {
	for _, detach := range []bool{false, true} {
		t.Run(fmt.Sprintf("detach=%t", detach), func(t *testing.T) {
			posts, reads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/apps":
					json.NewEncoder(w).Encode(map[string]any{"apps": []any{}})
				case "/api/v1/deployments":
					posts++
					var request api.DeployOptions
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.IdempotencyKey == "" {
						t.Errorf("submission missing retry key: %v", err)
					}
					json.NewEncoder(w).Encode(api.DeployResult{App: api.App{ID: "app-1", Slug: "app", Ref: "acme/app", Name: "App"}, Status: "queued", GoalID: "goal-1"})
				case "/api/v1/deployments/goal-1":
					reads++
					json.NewEncoder(w).Encode(api.DeploymentPage{Goal: api.DeploymentGoal{ID: "goal-1", Status: "succeeded", Stage: "complete"}, Events: []api.DeploymentEvent{{ID: "event-1", Type: "agent", Title: "Repository reviewed"}}, NextCursor: "cursor-1"})
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			control := &fakeSourceControl{repository: source.Repository{Root: "/work/app", Branch: "main", Revision: strings.Repeat("a", 40)}, publicURL: "https://github.com/acme/app.git", public: true}
			command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Credentials: &config.MemoryCredentialStore{}, Source: control, WorkingDir: func() (string, error) { return "/work/app", nil }, Getenv: func(string) string { return "" }})
			args := []string{"--api-url", server.URL, "--config", deployConfiguration(t, server.URL), "deploy", "--yes", "--json"}
			if detach {
				args = append(args, "--detach")
			}
			if code := command.Run(t.Context(), args); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			expectedReads, expectedLines := 1, 3
			if detach {
				expectedReads, expectedLines = 0, 1
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if posts != 1 || reads != expectedReads || len(lines) != expectedLines {
				t.Fatalf("posts=%d reads=%d output=%s", posts, reads, stdout.String())
			}
			for _, line := range lines {
				if !json.Valid([]byte(line)) {
					t.Errorf("invalid JSON line: %s", line)
				}
			}
		})
	}
}
