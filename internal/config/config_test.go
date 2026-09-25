package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStorePreservesRepositoryMappingsAcrossProfileUpdates(t *testing.T) {
	for _, repositories := range []string{
		`null`,
		`{}`,
		`{"http://localhost:4000":{"/work/app":{"id":"repo-1","branch":"main","metadata":{"sequence":9007199254740993,"enabled":true}}}}`,
	} {
		t.Run(repositories, func(t *testing.T) {
			store := Store{Path: filepath.Join(t.TempDir(), "config.json")}
			contents := `{"version":1,"current":"http://localhost:4000","profiles":{"http://localhost:4000":{"api_url":"http://localhost:4000","team_id":"old-team"}},"repositories":` + repositories + `}`
			if err := os.WriteFile(store.Path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			configuration, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !configuration.SetTeam("http://localhost:4000", "new-team", "acme", "acme", "Acme") {
				t.Fatal("existing profile was not loaded")
			}
			if err := store.Save(configuration); err != nil {
				t.Fatal(err)
			}
			reloaded, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			var preserved bytes.Buffer
			if err := json.Compact(&preserved, reloaded.Repositories); err != nil {
				t.Fatal(err)
			}
			if preserved.String() != repositories {
				t.Fatalf("repository mappings changed: got %s, want %s", preserved.String(), repositories)
			}
			profile, _ := reloaded.Profile("http://localhost:4000")
			if profile.TeamID != "new-team" || reloaded.Current != "http://localhost:4000" {
				t.Fatal("profile update or current installation was not preserved")
			}
		})
	}
}

func TestStoreRoundTripUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	store := Store{Path: path}
	want := Profile{
		APIURL:        "https://example.test",
		Issuer:        "https://example.test",
		Resource:      "https://example.test/api",
		ClientID:      "client-123456789",
		TokenEndpoint: "https://example.test/oauth/token",
		AccessToken:   "access-secret",
		RefreshToken:  "refresh-secret",
		TokenType:     "Bearer",
		ExpiresAt:     time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		Scopes:        []string{"apps:read", "logs:read"},
		TeamID:        "team-1",
		TeamSlug:      "acme",
		TeamRef:       "acme",
		TeamName:      "Acme",
	}
	configuration := Config{}
	configuration.SetProfile(want)

	if err := store.Save(configuration); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %04o, want 0600", info.Mode().Perm())
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	profile, ok := got.Profile(want.APIURL)
	if !ok {
		t.Fatal("Load() did not return saved profile")
	}
	if profile.RefreshToken != want.RefreshToken || profile.AccessToken != want.AccessToken {
		t.Fatalf("Load() profile tokens = %q/%q", profile.AccessToken, profile.RefreshToken)
	}
	if profile.TeamID != want.TeamID || profile.TeamSlug != want.TeamSlug || profile.TeamRef != want.TeamRef || profile.TeamName != want.TeamName {
		t.Fatalf("Load() profile team = %#v", profile)
	}
}

func TestSetTeamUpdatesOnlyMatchingProfile(t *testing.T) {
	configuration := Config{}
	configuration.SetProfile(Profile{APIURL: "https://one.example", TeamID: "old"})
	configuration.SetProfile(Profile{APIURL: "https://two.example", TeamID: "other"})

	if !configuration.SetTeam("https://one.example", "new", "new-team", "new-team", "New Team") {
		t.Fatal("SetTeam() = false")
	}
	first, _ := configuration.Profile("https://one.example")
	second, _ := configuration.Profile("https://two.example")
	if first.TeamID != "new" || first.TeamSlug != "new-team" || first.TeamRef != "new-team" || first.TeamName != "New Team" || second.TeamID != "other" {
		t.Fatalf("profiles after SetTeam() = %#v / %#v", first, second)
	}
	if configuration.SetTeam("https://missing.example", "team", "missing", "missing", "Missing") {
		t.Fatal("SetTeam() missing profile = true")
	}
}

func TestRemovingAuthenticationPreservesSelectedAPIURL(t *testing.T) {
	configuration := Config{}
	configuration.SetProfile(Profile{APIURL: "http://localhost:4000"})
	configuration.SetCurrentAPIURL("http://localhost:4000")

	if !configuration.RemoveProfile("http://localhost:4000") {
		t.Fatal("RemoveProfile() = false")
	}
	if configuration.Current != "http://localhost:4000" {
		t.Fatalf("Current = %q", configuration.Current)
	}
}

func TestStoreRejectsUnsafePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (Store{Path: path}).Load()
	if err == nil {
		t.Fatal("Load() error = nil, want unsafe permissions error")
	}
}

func TestNormalizeAPIURL(t *testing.T) {
	got, err := NormalizeAPIURL(" https://example.test/control/ ")
	if err != nil {
		t.Fatalf("NormalizeAPIURL() error = %v", err)
	}
	if got != "https://example.test/control" {
		t.Fatalf("NormalizeAPIURL() = %q", got)
	}
	if _, err := NormalizeAPIURL("file:///tmp/socket"); err == nil {
		t.Fatal("NormalizeAPIURL(file URL) error = nil")
	}
	for _, value := range []string{"http://localhost:4000/", "http://127.0.0.1:4000", "http://[::1]:4000", "http://LOCALHOST:4000"} {
		if _, err := NormalizeAPIURL(value); err != nil {
			t.Errorf("NormalizeAPIURL(%q) rejected loopback: %v", value, err)
		}
	}
	for _, value := range []string{"http://example.test", "http://192.0.2.1:4000", "http://localhost.example.test", "http://127.0.0.2.example.test"} {
		if _, err := NormalizeAPIURL(value); err == nil {
			t.Errorf("NormalizeAPIURL(%q) accepted cleartext remote API", value)
		}
	}
}
