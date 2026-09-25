package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/config"
)

// Exercise the whole public command with real Git initialization, commits,
// remote configuration and HTTP pushes. No source-control fakes are used.
func TestDeployFolderThroughUploadAndProgressThenRedeploy(t *testing.T) {
	for _, scenario := range []string{"fresh-folder", "unborn-repository", "unlinked-repository", "retry-upload", "confirm", "edit"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario == "unborn-repository" || scenario == "unlinked-repository" {
				folderDeployGit(t, directory, "init", "--initial-branch=main")
			}
			if scenario == "unlinked-repository" {
				folderDeployGit(t, directory, "add", "--all")
				folderDeployGit(t, directory, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--no-gpg-sign", "-m", "Existing source")
			}
			bareRoot := t.TempDir()
			bare := filepath.Join(bareRoot, "acme", "fresh.git")
			if err := os.MkdirAll(filepath.Dir(bare), 0o700); err != nil {
				t.Fatal(err)
			}
			folderDeployGit(t, bareRoot, "init", "--bare", "--initial-branch=main", bare)
			folderDeployGit(t, bare, "config", "http.receivepack", "true")
			gitExecutable, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			backend := &cgi.Handler{Path: gitExecutable, Args: []string{"http-backend"}, Root: "/git", Env: []string{"GIT_PROJECT_ROOT=" + bareRoot, "GIT_HTTP_EXPORT_ALL=1"}}
			team := api.Team{ID: "team-1", Slug: "acme", Ref: "acme", Name: "Acme"}
			app := api.App{ID: "app-1", Slug: "fresh", Ref: "acme/fresh", Name: "Fresh App", Team: team, Branch: "main", RepositoryURL: api.ManagedSourceURL}
			if scenario == "edit" {
				app.Name, app.Branch = "Edited App", "release"
			}
			created, submitted, watched := 0, 0, 0
			failUpload := scenario == "retry-upload"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer access" {
					t.Error("request is missing the existing login")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				switch {
				case r.Method == "POST" && r.URL.Path == "/api/v1/teams/acme/git-apps":
					created++
					var options api.CreateManagedAppOptions
					if err := json.NewDecoder(r.Body).Decode(&options); err != nil || options.Name != app.Name || options.Branch != app.Branch || options.Region != "" {
						t.Errorf("invalid app creation: %+v, %v", options, err)
					}
					json.NewEncoder(w).Encode(map[string]any{"app": api.ManagedApp{ID: app.ID, Ref: app.Ref, Name: app.Name, Branch: app.Branch, GitRemoteURL: "http://" + r.Host + "/git/acme/fresh.git"}})
				case r.Method == "GET" && r.URL.Path == "/api/v1/apps":
					json.NewEncoder(w).Encode(map[string]any{"apps": []api.App{app}})
				case strings.HasPrefix(r.URL.Path, "/git/"):
					if failUpload && r.Method == "POST" {
						failUpload = false
						http.Error(w, "temporary upload failure", http.StatusServiceUnavailable)
						return
					}
					backend.ServeHTTP(w, r)
				case r.Method == "POST" && r.URL.Path == "/api/v1/managed-deployments":
					submitted++
					var options api.DeployOptions
					if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
						t.Error(err)
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					// Verify the actual received objects before accepting deployment.
					revision, err := exec.CommandContext(r.Context(), "git", "-C", bare, "rev-parse", "refs/heads/"+app.Branch).Output()
					if err != nil || strings.TrimSpace(string(revision)) != options.Revision || options.AppID != app.ID || options.RepositoryURL != api.ManagedSourceURL {
						t.Errorf("deployment did not reference uploaded source: %+v, %v", options, err)
					}
					json.NewEncoder(w).Encode(api.DeployResult{App: app, GoalID: "goal-1", Status: "queued"})
				case r.Method == "GET" && r.URL.Path == "/api/v1/deployments/goal-1":
					http.Error(w, "goals scope unavailable", http.StatusForbidden)
				case r.Method == "GET" && r.URL.Path == "/api/v1/managed-deployments/goal-1":
					watched++
					json.NewEncoder(w).Encode(api.DeploymentPage{Goal: api.DeploymentGoal{ID: "goal-1", Status: "succeeded", Stage: "complete", Result: api.DeploymentOutcome{URL: "https://fresh.example"}}, Events: []api.DeploymentEvent{{ID: "build", Type: "stage", Title: "Build application", Status: "succeeded"}}, NextCursor: "c1"})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
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
			credentials := &config.MemoryCredentialStore{}
			// Cancelling must leave real Git state and server state untouched.
			var gitStatus, gitConfig string
			hasGit := scenario == "unborn-repository" || scenario == "unlinked-repository"
			if hasGit {
				gitStatus = folderDeployGit(t, directory, "status", "--porcelain")
				gitConfig = folderDeployGit(t, directory, "config", "--local", "--list")
			}
			var cancelledOutput bytes.Buffer
			cancelled := New(Dependencies{Stdin: strings.NewReader("c\n"), Stdout: &cancelledOutput, Stderr: &cancelledOutput, HTTPClient: server.Client(), Credentials: credentials, Getenv: func(string) string { return "" }, WorkingDir: func() (string, error) { return directory, nil }, IsTerminal: func() bool { return true }})
			if code := cancelled.Run(t.Context(), []string{"--api-url", server.URL, "--config", configPath, "deploy"}); code != 0 || !strings.Contains(cancelledOutput.String(), "Deployment cancelled") {
				t.Fatalf("cancel failed: %d %s", code, cancelledOutput.String())
			}
			if created != 0 || submitted != 0 {
				t.Fatal("cancellation created an application or deployment")
			}
			if hasGit {
				if folderDeployGit(t, directory, "status", "--porcelain") != gitStatus || folderDeployGit(t, directory, "config", "--local", "--list") != gitConfig {
					t.Fatal("cancellation changed Git state")
				}
			} else if _, err := os.Stat(filepath.Join(directory, ".git")); !os.IsNotExist(err) {
				t.Fatalf("cancellation initialized Git: %v", err)
			}
			for attempt := range 3 {
				if attempt == 2 {
					folderDeployGit(t, directory, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "--no-gpg-sign", "-m", "Next deployment")
				}
				var stdout, stderr bytes.Buffer
				interactive := attempt == 0 && (scenario == "confirm" || scenario == "edit")
				answers := "\n"
				if scenario == "edit" {
					answers = "e\n\nEdited App\nrelease\n\nd\n"
				}
				command := New(Dependencies{Stdin: strings.NewReader(answers), Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Credentials: credentials, Getenv: func(string) string { return "" }, WorkingDir: func() (string, error) { return directory, nil }, IsTerminal: func() bool { return interactive }})
				args := []string{"--api-url", server.URL, "--config", configPath, "deploy", "--name", "Fresh App", "--json"}
				if attempt == 0 && !interactive {
					args = append(args, "--yes")
				}
				code := command.Run(t.Context(), args)
				if scenario == "retry-upload" && attempt == 0 {
					if code == 0 || submitted != 0 {
						t.Fatal("failed upload was accepted as a deployment")
					}
					continue
				}
				if code != 0 {
					t.Fatalf("attempt %d failed: %s", attempt, stderr.String())
				}
				if attempt > 0 && strings.Contains(stderr.String(), "Review deployment") {
					t.Fatalf("redeploy prompted for confirmation: %s", stderr.String())
				}
				if interactive && scenario == "edit" && (strings.Count(stderr.String(), "Review deployment") != 2 || !strings.Contains(stderr.String(), "Application: Edited App (create new)") || !strings.Contains(stderr.String(), "Branch: release")) {
					t.Fatalf("edited settings were not reviewed again: %s", stderr.String())
				}
				for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
					if !json.Valid([]byte(line)) {
						t.Fatalf("invalid JSON output: %s", line)
					}
				}
				if !strings.Contains(stdout.String(), "https://fresh.example") {
					t.Fatalf("deployment did not reach the final progress result: %s", stdout.String())
				}
			}
			if created != 1 || submitted != watched || watched < 2 {
				t.Fatalf("app was duplicated or watcher bypassed: created=%d submitted=%d watched=%d", created, submitted, watched)
			}
			if remote := strings.TrimSpace(folderDeployGit(t, directory, "remote", "get-url", "infrastry")); remote != server.URL+"/git/acme/fresh.git" {
				t.Fatalf("deployment association was not retained: %s", remote)
			}
			if state := strings.TrimSpace(folderDeployGit(t, directory, "status", "--porcelain")); state != "" {
				t.Fatalf("deploy left uncommitted files: %s", state)
			}
		})
	}
}

func folderDeployGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v: %s", arguments, err, output)
	}
	return string(output)
}
