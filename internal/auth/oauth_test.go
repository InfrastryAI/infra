package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/InfrastryAI/infra/internal/config"
)

func TestTokenRequestsDoNotFollowRedirects(t *testing.T) {
	redirected := 0
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirected++
		if err := request.ParseForm(); err == nil && request.Form.Get("refresh_token") != "" {
			t.Error("refresh token reached redirect destination")
		}
	}))
	defer destination.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", destination.URL+"/stolen")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	var response tokenResponse
	if err := postForm(t.Context(), origin.Client(), origin.URL+"/token", url.Values{"refresh_token": {"refresh-secret"}}, &response); err == nil {
		t.Fatal("accepted redirected token response")
	}
	if redirected != 0 {
		t.Fatalf("followed token redirect %d times", redirected)
	}
}

func TestLoginCompletesPKCEBrowserFlow(t *testing.T) {
	serverScopes := []string{"goals:read", "goals:write", "apps:read", "logs:read", "offline_access", "networks:connect", "networks:manage", "databases:credentials:read", "future:tool"}
	for _, test := range []struct {
		name        string
		super       bool
		scopes      []string
		granted     string
		refresh     string
		want        []string
		wantError   string
		requested   []string
		unsupported string
	}{
		{name: "super includes all advertised scopes", super: true, granted: strings.Join(serverScopes, " "), refresh: "refresh-token", requested: serverScopes, want: serverScopes},
		{name: "network capability", scopes: []string{"network"}, requested: []string{"apps:read", "offline_access", "networks:connect", "networks:manage"}, refresh: "refresh-token", want: []string{"apps:read", "offline_access", "networks:connect", "networks:manage"}},
		{name: "database capability", scopes: []string{"database"}, requested: []string{"apps:read", "offline_access", "networks:connect", "networks:manage", "databases:credentials:read"}, refresh: "refresh-token", want: []string{"apps:read", "offline_access", "networks:connect", "networks:manage", "databases:credentials:read"}},
		{name: "server omits refresh permission", scopes: []string{" apps ", "apps"}, requested: []string{"apps:read", "offline_access"}, granted: "apps:read", want: []string{"apps:read"}},
		{name: "omitted response scope preserves expansion", scopes: []string{"logs"}, requested: []string{"apps:read", "offline_access", "logs:read"}, refresh: "refresh-token", want: []string{"apps:read", "offline_access", "logs:read"}},
		{name: "narrower consent recorded", scopes: []string{"apps", "logs"}, requested: []string{"apps:read", "offline_access", "logs:read"}, granted: "apps:read", want: []string{"apps:read"}},
		{name: "extra scope rejected", scopes: []string{"apps"}, requested: []string{"apps:read", "offline_access"}, granted: "apps:read networks:manage", wantError: "outside the requested scopes"},
		{name: "unsupported scope rejected before browser", scopes: []string{"database"}, unsupported: "databases:credentials:read", wantError: "databases:credentials:read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var registered, opened bool
			advertised := slices.DeleteFunc(slices.Clone(serverScopes), func(scope string) bool { return scope == test.unsupported })
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/.well-known/oauth-protected-resource":
					writeTestJSON(writer, map[string]any{"resource": server.URL + "/api", "authorization_servers": []string{server.URL}})
				case "/.well-known/oauth-authorization-server":
					writeTestJSON(writer, map[string]any{
						"issuer": server.URL, "authorization_endpoint": server.URL + "/oauth/authorize",
						"token_endpoint": server.URL + "/oauth/token", "registration_endpoint": server.URL + "/oauth/register",
						"scopes_supported": advertised,
					})
				case "/oauth/register":
					registered = true
					writeTestJSON(writer, map[string]any{"client_id": "test-client-123456"})
				case "/oauth/token":
					if err := request.ParseForm(); err != nil {
						t.Error(err)
						return
					}
					if request.Form.Get("grant_type") != "authorization_code" || request.Form.Get("code_verifier") == "" {
						t.Errorf("unexpected token form: %v", request.Form)
					}
					writeTestJSON(writer, map[string]any{
						"access_token": "access-token", "refresh_token": test.refresh,
						"token_type": "Bearer", "expires_in": 3600, "scope": test.granted,
					})
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

			profile, err := Login(t.Context(), LoginOptions{
				APIURL: server.URL, HTTPClient: server.Client(), Super: test.super, Scopes: test.scopes,
				Timeout: time.Second, Now: func() time.Time { return now },
				OpenURL: func(address string) error {
					opened = true
					authorize, err := url.Parse(address)
					if err != nil {
						return err
					}
					if authorize.Query().Get("code_challenge_method") != "S256" {
						t.Error("missing PKCE S256")
					}
					if got := authorize.Query().Get("scope"); got != strings.Join(test.requested, " ") {
						t.Errorf("scope = %q; want %v", got, test.requested)
					}
					callback, err := url.Parse(authorize.Query().Get("redirect_uri"))
					if err != nil {
						return err
					}
					query := callback.Query()
					query.Set("code", "authorization-code")
					query.Set("state", authorize.Query().Get("state"))
					callback.RawQuery = query.Encode()
					response, err := http.Get(callback.String())
					if err == nil {
						response.Body.Close()
					}
					return err
				},
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Login() = %#v, %v", profile, err)
				}
				if profile.AccessToken != "" {
					t.Fatal("failed login returned credentials")
				}
				if test.name == "unsupported scope rejected before browser" && (registered || opened) {
					t.Fatal("unsupported scope reached registration or browser")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if profile.AccessToken != "access-token" || profile.RefreshToken != test.refresh {
				t.Fatal("unexpected login tokens")
			}
			if !profile.ExpiresAt.Equal(now.Add(time.Hour)) {
				t.Fatalf("expiry = %s", profile.ExpiresAt)
			}
			if !slices.Equal(profile.Scopes, test.want) {
				t.Fatalf("scopes = %v; want %v", profile.Scopes, test.want)
			}
		})
	}
}

func TestTokenManagerRefreshesAndRotatesToken(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("refresh_token") != "old-refresh" {
			t.Errorf("refresh token = %q", request.Form.Get("refresh_token"))
		}
		writeTestJSON(writer, map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"token_type":    "Bearer",
			"expires_in":    120,
			"scope":         "apps:read logs:read offline_access",
		})
	}))
	defer server.Close()

	profile := config.Profile{
		ClientID:      "client-id",
		Resource:      server.URL + "/api",
		TokenEndpoint: server.URL,
		RefreshToken:  "old-refresh",
		ExpiresAt:     now.Add(-time.Minute),
	}
	var persisted config.Profile
	manager := TokenManager{
		Profile:    &profile,
		HTTPClient: server.Client(),
		Now:        func() time.Time { return now },
		Persist: func(profile config.Profile) error {
			persisted = profile
			return nil
		},
	}
	token, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "new-access" || persisted.RefreshToken != "new-refresh" {
		t.Fatalf("Token() = %q, persisted refresh = %q", token, persisted.RefreshToken)
	}
}

func writeTestJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func TestAccessOnlyTokenWorksUntilExpiry(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	profile := config.Profile{AccessToken: "access-only", ExpiresAt: now.Add(10 * time.Second)}
	manager := TokenManager{Profile: &profile, Now: func() time.Time { return now }}
	token, err := manager.Token(t.Context())
	if err != nil || token != "access-only" {
		t.Fatalf("Token() = %q, %v", token, err)
	}
	now = profile.ExpiresAt
	if _, err := manager.Token(t.Context()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("expired token error = %v", err)
	}
	if _, err := manager.Refresh(t.Context()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("refresh error = %v", err)
	}
}
