package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDatabaseCredentialsRequireSecureOrigin(t *testing.T) {
	client := Client{BaseURL: "http://example.test", Tokens: &fakeTokens{token: "test"}}
	if _, err := client.DatabaseCredentials(t.Context(), "session", "service"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatal("accepted insecure origin")
	}
}

func TestDatabaseCredentialsDoNotFollowRedirectsOrEchoErrorBodies(t *testing.T) {
	for _, status := range []int{200, 307, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/network-sessions/session/services/service/credentials" {
					t.Error("followed redirect")
				}
				if status == 307 {
					w.Header().Set("Location", "/redirected")
				}
				w.WriteHeader(status)
				if status == 200 {
					w.Write([]byte(`{"credentials":{"password":123456789}}`))
				} else {
					w.Write([]byte(`{"error":{"message":"123456789"}}`))
				}
			}))
			defer server.Close()
			client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: &fakeTokens{token: "test"}}
			_, err := client.DatabaseCredentials(t.Context(), "session", "service")
			if err == nil || strings.Contains(err.Error(), "123456789") {
				t.Fatal("credential response was accepted or exposed in error")
			}
		})
	}
}
