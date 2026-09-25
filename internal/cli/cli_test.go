package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/config"
	"github.com/InfrastryAI/infra/internal/source"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestLogsAllowsFlagsAfterApplicationAndPrintsOnePage(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/api/v1/apps":
			if request.URL.Query().Get("team") != "acme" {
				t.Errorf("team = %q", request.URL.Query().Get("team"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"apps": []map[string]any{{
				"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Acme API",
				"status": "healthy", "updated_at": now,
				"team": map[string]any{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
			}}})
		case "/api/v1/teams/acme/apps/customer-api/logs":
			if request.URL.Query().Get("tail") != "1" {
				t.Errorf("tail = %q", request.URL.Query().Get("tail"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"app": map[string]any{
					"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Acme API",
					"team": map[string]any{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
				},
				"logs": []map[string]any{{
					"id": "log-1", "app_id": "app-1", "level": "info", "source": "runtime", "dataset": "runtime",
					"component_name": "web", "message": "ready", "occurred_at": now, "observed_at": now,
				}}, "next_cursor": "cursor-1",
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	configurationPath := filepath.Join(t.TempDir(), "config.json")
	store := config.Store{Path: configurationPath}
	configuration := config.Config{}
	configuration.SetProfile(config.Profile{
		APIURL: server.URL, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
		TeamID: "team-1", TeamSlug: "acme", TeamRef: "acme", TeamName: "Acme",
	})
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Now: func() time.Time { return now },
		Getenv: func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", server.URL, "--config", configurationPath,
		"logs", "acme/customer-api", "--follow=false", "--tail", "1",
	})
	if exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ready") || !strings.Contains(stdout.String(), "runtime") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestLogsSilentlyReconnectsAfterOneTransportInterruption(t *testing.T) {
	now := time.Date(2026, 8, 31, 15, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload any
		switch request.URL.Path {
		case "/api/v1/apps":
			payload = map[string]any{"apps": []map[string]any{{
				"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Acme API",
				"status": "healthy", "updated_at": now,
				"team": map[string]any{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
			}}}
		case "/api/v1/teams/acme/apps/customer-api/logs":
			logRequests++
			switch logRequests {
			case 1:
				payload = map[string]any{"logs": []any{}, "next_cursor": "cursor-1"}
			case 2:
				return nil, errors.New("connection reset by peer")
			case 3:
				payload = map[string]any{
					"logs": []map[string]any{{
						"id": "log-1", "app_id": "app-1", "level": "info", "source": "runtime", "dataset": "runtime",
						"component_name": "web", "message": "reconnected", "occurred_at": now, "observed_at": now,
					}},
					"next_cursor": "cursor-2",
				}
			default:
				cancel()
				return nil, context.Canceled
			}
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
				Request:    request,
			}, nil
		}

		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode response: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    request,
		}, nil
	})}

	configurationPath := filepath.Join(t.TempDir(), "config.json")
	store := config.Store{Path: configurationPath}
	configuration := config.Config{}
	configuration.SetProfile(config.Profile{
		APIURL: "https://example.test", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
		TeamID: "team-1", TeamSlug: "acme", TeamRef: "acme", TeamName: "Acme",
	})
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: client, Now: func() time.Time { return now },
		Getenv: func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(ctx, []string{
		"--api-url", "https://example.test", "--config", configurationPath,
		"logs", "acme/customer-api", "--tail", "0", "--poll", "200ms",
	})
	if exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "reconnected") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "connection reset by peer") || strings.Contains(stderr.String(), "temporarily unavailable") {
		t.Fatalf("one transient failure should be silent, stderr = %q", stderr.String())
	}
	if logRequests != 4 {
		t.Fatalf("log requests = %d", logRequests)
	}
}

func TestAppsSelectsAndPersistsDefaultTeam(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/teams":
			_ = json.NewEncoder(writer).Encode(map[string]any{"teams": []map[string]any{
				{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
				{"id": "team-2", "slug": "beta", "ref": "beta", "name": "Beta"},
			}})
		case "/api/v1/apps":
			if request.URL.Query().Get("include") != "containers" {
				t.Error("apps list did not request containers")
			}
			if request.URL.Query().Get("team") != "acme" {
				t.Errorf("team = %q", request.URL.Query().Get("team"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"apps": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	configurationPath := filepath.Join(t.TempDir(), "config.json")
	store := config.Store{Path: configurationPath}
	configuration := config.Config{}
	configuration.SetProfile(config.Profile{
		APIURL: server.URL, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
	})
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Now: func() time.Time { return now },
		Getenv: func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", server.URL, "--config", configurationPath, "apps", "list",
	})
	if exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Selected default team acme (Acme).") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := saved.Profile(server.URL)
	if profile.TeamID != "team-1" || profile.TeamSlug != "acme" || profile.TeamRef != "acme" || profile.TeamName != "Acme" {
		t.Fatalf("saved team = %#v", profile)
	}
}

func TestConfigSetTeamChangesContextByName(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/teams" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"teams": []map[string]any{
			{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
			{"id": "team-2", "slug": "beta", "ref": "beta", "name": "Beta"},
		}})
	}))
	defer server.Close()

	configurationPath := filepath.Join(t.TempDir(), "config.json")
	store := config.Store{Path: configurationPath}
	configuration := config.Config{}
	configuration.SetProfile(config.Profile{
		APIURL: server.URL, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
		TeamID: "team-1", TeamSlug: "acme", TeamRef: "acme", TeamName: "Acme",
	})
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	credentials := &config.MemoryCredentialStore{}
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Now: func() time.Time { return now },
		Getenv: func(string) string { return "" }, Credentials: credentials,
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", server.URL, "--config", configurationPath, "config", "set", "team", "beta",
	})
	if exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if stdout.String() != "Set team to beta (Beta).\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := saved.Profile(server.URL)
	if profile.TeamID != "team-2" || profile.TeamSlug != "beta" || profile.TeamRef != "beta" || profile.TeamName != "Beta" {
		t.Fatalf("saved team = %#v", profile)
	}

	stdout.Reset()
	stderr.Reset()
	command = New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Now: func() time.Time { return now },
		Getenv: func(string) string { return "" }, Credentials: credentials,
	})
	exitCode = command.Run(t.Context(), []string{
		"--api-url", server.URL, "--config", configurationPath, "config", "get", "team",
	})
	if exitCode != 0 {
		t.Fatalf("config get team exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if stdout.String() != "beta (Beta)\n" {
		t.Fatalf("config get team stdout = %q", stdout.String())
	}
}

func TestResolveAppRejectsAmbiguousNames(t *testing.T) {
	_, err := resolveApp([]api.App{
		{ID: "one", Slug: "web", Ref: "acme/web", Name: "Web"},
		{ID: "two", Slug: "web-2", Ref: "acme/web-2", Name: "web"},
	}, "WEB")
	if err == nil || !strings.Contains(err.Error(), "acme/web, acme/web-2") {
		t.Fatalf("resolveApp() error = %v", err)
	}
}

func TestResolveTeamRejectsAmbiguousNames(t *testing.T) {
	_, err := resolveTeam([]api.Team{
		{ID: "one", Slug: "acme", Ref: "acme", Name: "Acme"},
		{ID: "two", Slug: "acme-consulting", Ref: "acme-consulting", Name: "acme"},
	}, "ACME")
	if err == nil || !strings.Contains(err.Error(), "acme, acme-consulting") {
		t.Fatalf("resolveTeam() error = %v", err)
	}
}

func TestResolveAppAcceptsSlugAndFullRefButNotUUID(t *testing.T) {
	apps := []api.App{{ID: "6cf22aa8-1ffc-4d74-958b-1dbe74978c4f", Slug: "customer-api", Ref: "acme/customer-api", Name: "Customer API"}}
	for _, selector := range []string{"customer-api", "acme/customer-api"} {
		app, err := resolveApp(apps, selector)
		if err != nil || app.Ref != "acme/customer-api" {
			t.Fatalf("resolveApp(%q) = %#v, %v", selector, app, err)
		}
	}
	if _, err := resolveApp(apps, apps[0].ID); err == nil {
		t.Fatal("resolveApp(UUID) error = nil")
	}
}

func TestResolveTeamAcceptsRefButNotUUID(t *testing.T) {
	teams := []api.Team{{ID: "9d8ea255-41f0-4d6d-8703-faf741f5bada", Slug: "acme", Ref: "acme", Name: "Acme"}}
	team, err := resolveTeam(teams, "acme")
	if err != nil || team.Ref != "acme" {
		t.Fatalf("resolveTeam(acme) = %#v, %v", team, err)
	}
	if _, err := resolveTeam(teams, teams[0].ID); err == nil {
		t.Fatal("resolveTeam(UUID) error = nil")
	}
}

func TestPrintAppsUsesSlugAsPrimaryIdentifier(t *testing.T) {
	var output bytes.Buffer
	command := New(Dependencies{Stdout: &output})
	command.printApps([]api.App{{
		ID: "6cf22aa8-1ffc-4d74-958b-1dbe74978c4f", Slug: "customer-api", Ref: "acme/customer-api",
		Name: "Customer API", Status: "healthy", Region: "nyc",
		Team: api.Team{ID: "9d8ea255-41f0-4d6d-8703-faf741f5bada", Slug: "acme", Ref: "acme", Name: "Acme"},
	}})
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "SLUG") || !strings.HasPrefix(lines[1], "customer-api") {
		t.Fatalf("app table = %q", output.String())
	}
	if strings.Contains(output.String(), "6cf22aa8") || !strings.Contains(output.String(), "acme") {
		t.Fatalf("app table = %q", output.String())
	}
	if !regexp.MustCompile(`STATUS {2,}REGION {2,}TEAM {2,}URL`).MatchString(output.String()) ||
		!regexp.MustCompile(`customer-api {2,}Customer API`).MatchString(output.String()) {
		t.Fatalf("app table columns are not separated = %q", output.String())
	}
}

func TestCommandHelpDoesNotRequireAuthentication(t *testing.T) {
	for _, arguments := range [][]string{{"apps", "--help"}, {"logs", "tail", "--help"}} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command := New(Dependencies{
			Stdout: &stdout, Stderr: &stderr,
			Getenv: func(name string) string {
				if name == "INFRASTRY_CONFIG" {
					return filepath.Join(t.TempDir(), "config.json")
				}
				return ""
			},
		})
		if exitCode := command.Run(t.Context(), arguments); exitCode != 0 {
			t.Fatalf("Run(%v) exit = %d, stderr = %s", arguments, exitCode, stderr.String())
		}
		if !strings.Contains(stdout.String()+stderr.String(), "Usage:") {
			t.Fatalf("Run(%v) output = %q / %q", arguments, stdout.String(), stderr.String())
		}
	}
}

func TestCompletionScriptsDoNotRequireConfiguration(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command := New(Dependencies{
				Stdout: &stdout,
				Stderr: &stderr,
				Getenv: func(name string) string {
					if name == "INFRASTRY_CONFIG" {
						return filepath.Join(t.TempDir(), "missing", "config.json")
					}
					return ""
				},
			})
			if exitCode := command.Run(t.Context(), []string{"completion", shell}); exitCode != 0 {
				t.Fatalf("Run() exit = %d, stderr = %s", exitCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), "__complete") || !strings.Contains(stdout.String(), "infra") {
				t.Fatalf("completion script = %q", stdout.String())
			}
		})
	}
}

func TestCompletionCandidates(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      []string
	}{
		{name: "root", arguments: []string{"__complete", "a"}, want: []string{"apps", "auth"}},
		{name: "auth command", arguments: []string{"__complete", "auth", "l"}, want: []string{"login", "logout"}},
		{name: "config keys", arguments: []string{"__complete", "config", "get", ""}, want: []string{"api", "team"}},
		{name: "app status", arguments: []string{"__complete", "apps", "--status", "he"}, want: []string{"healthy"}},
		{name: "log dataset", arguments: []string{"__complete", "logs", "--dataset", "r"}, want: []string{"runtime"}},
		{name: "shell", arguments: []string{"__complete", "completion", "p"}, want: []string{"powershell"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			command := New(Dependencies{
				Stdout: &stdout,
				Stderr: &stderr,
				Getenv: func(name string) string {
					if name == "INFRASTRY_CONFIG" {
						return filepath.Join(t.TempDir(), "config.json")
					}
					return ""
				},
				Credentials: &config.MemoryCredentialStore{},
			})
			if exitCode := command.Run(t.Context(), test.arguments); exitCode != 0 {
				t.Fatalf("Run(%q) exit = %d, stderr = %q", test.arguments, exitCode, stderr.String())
			}
			got := stdout.String()
			for _, wanted := range test.want {
				if !strings.Contains(got, wanted) {
					t.Errorf("completion output for %q = %q, missing %q", test.arguments, got, wanted)
				}
			}
		})
	}
}

func TestDynamicCompletionSuggestsAuthenticatedApplications(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/apps" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"apps": []map[string]any{{
			"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Customer API",
			"team": map[string]any{"id": "team-1", "slug": "acme", "ref": "acme", "name": "Acme"},
		}}})
	}))
	defer server.Close()

	configurationPath := deployConfiguration(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(),
		Credentials: &config.MemoryCredentialStore{},
		Getenv: func(name string) string {
			if name == "INFRASTRY_CONFIG" {
				return configurationPath
			}
			return ""
		},
	})
	if exitCode := command.Run(t.Context(), []string{"__complete", "logs", "cust"}); exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "customer-api\tCustomer API") || !strings.Contains(stdout.String(), "acme/customer-api\tCustomer API") {
		t.Fatalf("completion output = %q", stdout.String())
	}
}

func TestCredentialsMigrateToKeyringAndLeaveConfigRedacted(t *testing.T) {
	configurationPath := filepath.Join(t.TempDir(), "config.json")
	store := config.Store{Path: configurationPath}
	configuration := config.Config{}
	configuration.SetProfile(config.Profile{
		APIURL: "https://example.test", AccessToken: "access-secret", RefreshToken: "refresh-secret",
		ExpiresAt: time.Now().Add(time.Hour), Scopes: []string{"apps:read"},
	})
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}

	credentials := &config.MemoryCredentialStore{}
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, Credentials: credentials})
	if exitCode := command.Run(t.Context(), []string{
		"--api-url", "https://example.test", "--config", configurationPath, "auth", "status",
	}); exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %q", exitCode, stderr.String())
	}

	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := saved.Profile("https://example.test")
	if profile.AccessToken != "" || profile.RefreshToken != "" {
		t.Fatalf("saved profile still contains tokens: %#v", profile)
	}
	secret, err := credentials.Get("https://example.test")
	if err != nil || secret.AccessToken != "access-secret" || secret.RefreshToken != "refresh-secret" {
		t.Fatalf("keyring credentials = %#v, %v", secret, err)
	}
}

type unavailableCredentialStore struct{ err error }

func (store unavailableCredentialStore) Get(string) (config.Credentials, error) {
	return config.Credentials{}, store.err
}

func (store unavailableCredentialStore) Set(string, config.Credentials) error { return store.err }
func (store unavailableCredentialStore) Delete(string) error                  { return store.err }

func TestCredentialsRemainInPrivateConfigWhenKeyringIsUnavailable(t *testing.T) {
	configurationPath := filepath.Join(t.TempDir(), "config.json")
	store := config.Store{Path: configurationPath}
	configuration := config.Config{}
	configuration.SetProfile(config.Profile{
		APIURL: "https://example.test", AccessToken: "access-secret", RefreshToken: "refresh-secret",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr,
		Credentials: unavailableCredentialStore{err: errors.New("keyring unavailable")},
	})
	if exitCode := command.Run(t.Context(), []string{
		"--api-url", "https://example.test", "--config", configurationPath, "auth", "status",
	}); exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %q", exitCode, stderr.String())
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := saved.Profile("https://example.test")
	if profile.AccessToken != "access-secret" || profile.RefreshToken != "refresh-secret" {
		t.Fatalf("fallback profile tokens = %q/%q", profile.AccessToken, profile.RefreshToken)
	}
}

func TestAccessibleTeamPromptSelectsRequestedTeam(t *testing.T) {
	var output bytes.Buffer
	command := New(Dependencies{
		Stdin: strings.NewReader("2\n"), Stderr: &output,
		IsTerminal: func() bool { return true },
		Getenv: func(name string) string {
			if name == "INFRASTRY_ACCESSIBLE" {
				return "1"
			}
			return ""
		},
	})
	team, err := command.selectTeam(t.Context(), []api.Team{
		{ID: "one", Slug: "one", Ref: "one", Name: "One"},
		{ID: "two", Slug: "two", Ref: "two", Name: "Two"},
	}, "Select a team")
	if err != nil {
		t.Fatal(err)
	}
	if team.Ref != "two" || !strings.Contains(output.String(), "2. Two (two)") {
		t.Fatalf("selected team/output = %#v/%q", team, output.String())
	}
}

func TestConfigSetAPIURLPersistsWithoutAuthentication(t *testing.T) {
	configurationPath := filepath.Join(t.TempDir(), "config.json")
	dependencies := Dependencies{
		Getenv: func(name string) string {
			if name == "INFRASTRY_CONFIG" {
				return configurationPath
			}
			return ""
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	dependencies.Stdout = &stdout
	dependencies.Stderr = &stderr
	command := New(dependencies)
	if exitCode := command.Run(t.Context(), []string{"config", "set", "API", "http://localhost:4000/"}); exitCode != 0 {
		t.Fatalf("config set api exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if stdout.String() != "Set api to http://localhost:4000.\n" {
		t.Fatalf("config set api stdout = %q", stdout.String())
	}

	saved, err := (config.Store{Path: configurationPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Current != "http://localhost:4000" {
		t.Fatalf("saved current API URL = %q", saved.Current)
	}

	stdout.Reset()
	stderr.Reset()
	command = New(dependencies)
	if exitCode := command.Run(t.Context(), []string{"config", "get", "api"}); exitCode != 0 {
		t.Fatalf("config get api exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if stdout.String() != "http://localhost:4000\n" {
		t.Fatalf("config get api stdout = %q", stdout.String())
	}
}

type fakeSourceControl struct {
	repository       source.Repository
	inspectError     error
	publicURL        string
	public           bool
	initialized      bool
	ensuredRemote    string
	ensuredRemoteURL string
	pushed           bool
}

func (control *fakeSourceControl) Inspect(context.Context, string, string, string) (source.Repository, error) {
	return control.repository, control.inspectError
}

func (control *fakeSourceControl) Initialize(context.Context, string, string, string) (source.Repository, error) {
	control.initialized = true
	return control.repository, nil
}

func (control *fakeSourceControl) PublicRemote(context.Context, source.Repository) (string, bool) {
	return control.publicURL, control.public
}

func (control *fakeSourceControl) EnsureRemote(_ context.Context, _ source.Repository, name, repositoryURL string) error {
	control.ensuredRemote = name
	control.ensuredRemoteURL = repositoryURL
	return nil
}

func (control *fakeSourceControl) Push(context.Context, source.Repository, string, string) error {
	control.pushed = true
	return nil
}

func TestDeployUsesPublicRepositoryAtCurrentCommit(t *testing.T) {
	revision := "0123456789abcdef0123456789abcdef01234567"
	var deployment api.DeployOptions
	repositoryRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/apps":
			if request.URL.Query().Get("team") != "acme" {
				t.Errorf("team = %q", request.URL.Query().Get("team"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"apps": []any{}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/deployments":
			if err := json.NewDecoder(request.Body).Decode(&deployment); err != nil {
				t.Errorf("decode deployment: %v", err)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"app": map[string]any{
					"id": "app-1", "slug": "customer-api", "ref": "acme/customer-api", "name": "Customer API",
				},
				"status": "queued", "goal_id": "goal-1",
			})
		case request.URL.Path == "/api/v1/repositories":
			repositoryRequests++
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	configurationPath := deployConfiguration(t, server.URL)
	control := &fakeSourceControl{
		repository: source.Repository{Root: "/work/customer-api", Branch: "main", Revision: revision},
		publicURL:  "https://github.com/acme/customer-api.git",
		public:     true,
	}
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Source: control,
		WorkingDir: func() (string, error) { return "/work/customer-api", nil },
		Getenv:     func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", server.URL, "--config", configurationPath, "deploy", "--yes", "--name", "Customer API", "--detach",
	})
	if exitCode != 0 {
		t.Fatalf("Run() exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if repositoryRequests != 0 || control.pushed {
		t.Fatalf("hosted repository requests/push = %d/%t", repositoryRequests, control.pushed)
	}
	if deployment.RepositoryURL != control.publicURL || deployment.Revision != revision || deployment.TeamID != "team-1" || deployment.Region != "" {
		t.Fatalf("deployment = %#v", deployment)
	}
	if !strings.Contains(stdout.String(), "Deployment queued for Customer API (acme/customer-api).") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDeployRejectsPrivateSourceWithoutCallingAPI(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		http.NotFound(writer, request)
	}))
	defer server.Close()

	configurationPath := deployConfiguration(t, server.URL)
	control := &fakeSourceControl{repository: source.Repository{
		Root: "/work/customer-api", Branch: "main", Revision: "abcdef0123456789abcdef0123456789abcdef01",
		RemoteURL: "git@github.com:acme/private-api.git",
	}}
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Source: control,
		WorkingDir: func() (string, error) { return "/work/customer-api", nil },
		Getenv:     func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", server.URL, "--config", configurationPath, "deploy",
	})
	if exitCode != 1 || !strings.Contains(stderr.String(), "only public GitHub repositories") {
		t.Fatalf("Run() exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	if requests != 0 || control.pushed || control.ensuredRemote != "" {
		t.Fatalf("requests/control = %d/%#v", requests, control)
	}
	if control.initialized {
		t.Fatal("private source was initialized")
	}
}

func TestDeployRejectsPublicNonGitHubRepository(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		http.NotFound(writer, request)
	}))
	defer server.Close()

	configurationPath := deployConfiguration(t, server.URL)
	control := &fakeSourceControl{
		repository: source.Repository{Root: "/work/app", Branch: "main", Revision: "0123456789abcdef"},
		publicURL:  "https://gitlab.com/acme/app.git",
		public:     true,
	}
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, HTTPClient: server.Client(), Source: control,
		WorkingDir: func() (string, error) { return "/work/app", nil },
		Getenv:     func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", server.URL, "--config", configurationPath, "deploy",
	})
	if exitCode != 1 || !strings.Contains(stderr.String(), "only public GitHub repositories") || requests != 0 {
		t.Fatalf("Run() exit/stdout/stderr/requests = %d/%q/%q/%d", exitCode, stdout.String(), stderr.String(), requests)
	}
}

func TestDeployChecksUploadPermissionBeforeInitializingDirectory(t *testing.T) {
	configurationPath := deployConfiguration(t, "https://api.infrastry.test")
	control := &fakeSourceControl{inspectError: source.ErrNotRepository}
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, Source: control,
		WorkingDir: func() (string, error) { return "/work/new-app", nil },
		Getenv:     func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", "https://api.infrastry.test", "--config", configurationPath, "deploy",
	})
	if exitCode != 1 || !strings.Contains(stderr.String(), "permission to upload") || control.initialized {
		t.Fatalf("Run() exit/stdout/stderr/initialized = %d/%q/%q/%t", exitCode, stdout.String(), stderr.String(), control.initialized)
	}
}

func TestDeployRemovedFlagsAreUnavailable(t *testing.T) {
	for _, flag := range []string{"--init", "--region", "--app"} {
		t.Run(flag, func(t *testing.T) {
			configurationPath := deployConfiguration(t, "https://api.infrastry.test")
			var stdout, stderr bytes.Buffer
			command := New(Dependencies{
				Stdout: &stdout, Stderr: &stderr,
				Getenv: func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
			})
			exitCode := command.Run(t.Context(), []string{
				"--api-url", "https://api.infrastry.test", "--config", configurationPath, "deploy", flag,
			})
			if exitCode != 2 || !strings.Contains(stderr.String(), "unknown flag: "+flag) {
				t.Fatalf("Run() exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
			}
		})
	}
}

func TestDeployRejectsDirtyWorkingTree(t *testing.T) {
	configurationPath := deployConfiguration(t, "https://api.infrastry.test")
	control := &fakeSourceControl{repository: source.Repository{
		Root: "/work/app", Branch: "main", Revision: "0123456789abcdef", Dirty: true,
	}}
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{
		Stdout: &stdout, Stderr: &stderr, Source: control,
		WorkingDir: func() (string, error) { return "/work/app", nil },
		Getenv:     func(string) string { return "" }, Credentials: &config.MemoryCredentialStore{},
	})
	exitCode := command.Run(t.Context(), []string{
		"--api-url", "https://api.infrastry.test", "--config", configurationPath, "deploy",
	})
	if exitCode != 1 || !strings.Contains(stderr.String(), "uncommitted or untracked") {
		t.Fatalf("Run() exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestMatchingAppIDNormalizesRepositoryURL(t *testing.T) {
	id, err := matchingAppID([]api.App{{
		ID: "app-1", Slug: "app", Ref: "acme/app", RepositoryURL: "https://CODE.example/acme/app.git", Branch: "main",
	}}, "https://code.example/acme/app/", "main")
	if err != nil || id != "app-1" {
		t.Fatalf("matchingAppID() = %q, %v", id, err)
	}
}

func deployConfiguration(t *testing.T, apiURL string) string {
	t.Helper()
	configurationPath := filepath.Join(t.TempDir(), "config.json")
	store := config.Store{Path: configurationPath}
	configuration := config.Config{}
	configuration.SetProfile(config.Profile{
		APIURL: apiURL, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
		Scopes: []string{"apps:read", "logs:read", "offline_access"},
		TeamID: "team-1", TeamSlug: "acme", TeamRef: "acme", TeamName: "Acme",
	})
	if err := store.Save(configuration); err != nil {
		t.Fatal(err)
	}
	return configurationPath
}

var _ source.Control = (*fakeSourceControl)(nil)

func TestPrintAppsRunningContainers(t *testing.T) {
	var output bytes.Buffer
	command := New(Dependencies{Stdout: &output})
	command.printApps([]api.App{
		{Slug: "api", Containers: []api.Container{
			{ID: "web-1", Name: "web", Status: "Running", Runtime: "Node.js"},
			{ID: "worker-1", Name: "worker", Status: "Up 2 minutes", Runtime: "Elixir · OTP 27"},
		}},
		{Slug: "empty", Containers: []api.Container{}},
		{Slug: "failed", ContainersError: "Try again"},
		{Slug: "unknown"},
		{Slug: "single", Containers: []api.Container{{ID: "job-1", Name: "job", Status: "Running"}}},
	})
	patterns := []string{
		`^SLUG\s+NAME / RUNTIME\s+STATUS`,
		`^api\s`,
		`^  ├─ web\s+Node\.js\s+Running\s*$`,
		`^  └─ worker\s+Elixir · OTP 27\s+Up 2 minutes\s*$`,
		`^empty\s`,
		`^  └─ No running containers\s*$`,
		`^failed\s`,
		`^  └─ Containers unavailable: Try again\s*$`,
		`^unknown\s`,
		`^  └─ Containers unavailable\s*$`,
		`^single\s`,
		`^  └─ job\s+—\s+Running\s*$`,
	}
	lines := strings.Split(strings.TrimRight(output.String(), "\n"), "\n")
	if len(lines) != len(patterns) {
		t.Fatalf("unexpected tree rows: %s", output.String())
	}
	for index, pattern := range patterns {
		if !regexp.MustCompile(pattern).MatchString(lines[index]) {
			t.Errorf("row %d = %q, want pattern %q", index, lines[index], pattern)
		}
	}
}
