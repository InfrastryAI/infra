package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/InfrastryAI/infra/internal/config"
)

func TestLoginFlagsRejectInvalidSelectionBeforeNetwork(t *testing.T) {
	for _, flags := range [][]string{
		nil, {"--super", "--scope", "apps"}, {"--scope", ""},
		{"--scope", "apps,"}, {"--scope", "apps logs"}, {"--scope", "apps:read"}, {"--scope", "unknown"},
		{"--network"}, {"--database"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			var output bytes.Buffer
			command := New(Dependencies{
				Stdout: &output, Stderr: &output, Getenv: func(string) string { return "" },
				Credentials: &config.MemoryCredentialStore{},
				HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Error("invalid selection made an HTTP request")
					return nil, errors.New("unexpected request")
				})},
				OpenURL: func(string) error { t.Error("invalid selection opened browser"); return nil },
			})
			args := append([]string{"--config", filepath.Join(t.TempDir(), "config.json"), "auth", "login"}, flags...)
			if code := command.Run(t.Context(), args); code == 0 {
				t.Fatal("invalid selection succeeded")
			}
			if len(flags) == 0 && !strings.Contains(output.String(), "--super (recommended)") {
				t.Fatalf("missing recommended login: %s", &output)
			}
		})
	}
}

func TestRestrictedLoginPersistsAndWorksWithoutRefresh(t *testing.T) {
	for _, keyringAvailable := range []bool{true, false} {
		name := "keyring"
		if !keyringAvailable {
			name = "private file"
		}
		t.Run(name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload any
				switch r.URL.Path {
				case "/.well-known/oauth-protected-resource":
					payload = map[string]any{"resource": server.URL + "/api", "authorization_servers": []string{server.URL}}
				case "/.well-known/oauth-authorization-server":
					payload = map[string]any{
						"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize",
						"registration_endpoint": server.URL + "/register", "token_endpoint": server.URL + "/token",
						"scopes_supported": []string{"apps:read", "logs:read", "offline_access", "networks:manage"},
					}
				case "/register":
					payload = map[string]string{"client_id": "test-client"}
				case "/token":
					if err := r.ParseForm(); err != nil {
						t.Error(err)
						return
					}
					if r.Form.Get("grant_type") != "authorization_code" {
						t.Error("unexpected refresh")
					}
					payload = map[string]any{"access_token": "access-only", "token_type": "Bearer", "expires_in": 60, "scope": "apps:read logs:read"}
				case "/api/v1/teams":
					if r.Header.Get("Authorization") != "Bearer access-only" {
						t.Error("missing access token")
					}
					payload = map[string]any{"teams": []any{}}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()
			var credentials config.CredentialStore = &config.MemoryCredentialStore{}
			if !keyringAvailable {
				credentials = unavailableCredentialStore{err: errors.New("unavailable")}
			}
			path := filepath.Join(t.TempDir(), "config.json")
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			var output bytes.Buffer
			dependencies := Dependencies{
				Stdout: &output, Stderr: &output, HTTPClient: server.Client(), Credentials: credentials,
				Getenv: func(string) string { return "" }, Now: func() time.Time { return now },
				OpenURL: func(address string) error {
					authorize, err := url.Parse(address)
					if err != nil {
						return err
					}
					if got := authorize.Query().Get("scope"); got != "apps:read offline_access logs:read" {
						t.Errorf("requested scope = %q", got)
					}
					callback, err := url.Parse(authorize.Query().Get("redirect_uri"))
					if err != nil {
						return err
					}
					values := callback.Query()
					values.Set("state", authorize.Query().Get("state"))
					values.Set("code", "test-code")
					callback.RawQuery = values.Encode()
					response, err := http.Get(callback.String())
					if err == nil {
						response.Body.Close()
					}
					return err
				},
			}
			run := func(args ...string) int {
				output.Reset()
				return New(dependencies).Run(t.Context(), append([]string{"--config", path, "--api-url", server.URL}, args...))
			}
			if code := run("auth", "login", "--scope", "apps,logs", "--scope", "apps", "--timeout", "1s"); code != 0 {
				t.Fatalf("login: %s", &output)
			}
			stored, err := (config.Store{Path: path}).Load()
			if err != nil {
				t.Fatal(err)
			}
			profile, _ := stored.Profile(server.URL)
			if !slices.Equal(profile.Scopes, []string{"apps:read", "logs:read"}) || profile.RefreshToken != "" {
				t.Fatalf("unexpected saved profile: %#v", profile)
			}
			if keyringAvailable {
				secret, err := credentials.Get(server.URL)
				if err != nil || secret.AccessToken != "access-only" || profile.AccessToken != "" {
					t.Fatal("access token not stored privately in keyring")
				}
			} else if profile.AccessToken != "access-only" {
				t.Fatal("access token not saved in private file")
			}
			if code := run("teams", "list", "--json"); code != 0 {
				t.Fatalf("authenticated command: %s", &output)
			}
			if code := run("auth", "status", "--json"); code != 0 || !strings.Contains(output.String(), `"status": "active"`) {
				t.Fatalf("active status: %s", &output)
			}
			now = now.Add(time.Minute)
			if code := run("auth", "status", "--json"); code != 0 || !strings.Contains(output.String(), `"status": "expired"`) {
				t.Fatalf("expired status: %s", &output)
			}
			if code := run("teams", "list"); code == 0 || !strings.Contains(output.String(), "authentication required") {
				t.Fatalf("expired login should require sign-in: %s", &output)
			}
		})
	}
}
