package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfrastryAI/infra/internal/config"
)

func TestExistingRepositoryMappingsSurviveCredentialMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const contents = `{"version":1,"current":"http://localhost:4000","profiles":{"http://localhost:4000":{"api_url":"http://localhost:4000","access_token":"test-access","refresh_token":"test-refresh"}},"repositories":{"saved-repository":{"id":"repo-1"}}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := &config.MemoryCredentialStore{}
	var stdout, stderr bytes.Buffer
	command := New(Dependencies{Stdout: &stdout, Stderr: &stderr, Credentials: credentials, Getenv: func(string) string { return "" }})
	code := command.Run(t.Context(), []string{"--config", path, "--api-url", "http://localhost:4000", "config", "get", "api"})
	if code != 0 || strings.TrimSpace(stdout.String()) != "http://localhost:4000" {
		t.Fatalf("exit %d: %s / %s", code, stdout.String(), stderr.String())
	}
	saved, err := (config.Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(saved.Repositories, []byte(`"repo-1"`)) {
		t.Fatal("credential migration discarded repository mappings")
	}
	profile, _ := saved.Profile("http://localhost:4000")
	if profile.AccessToken != "" || profile.RefreshToken != "" {
		t.Fatal("credential migration did not remove tokens from the config")
	}
	migrated, err := credentials.Get("http://localhost:4000")
	if err != nil || migrated.AccessToken != "test-access" || migrated.RefreshToken != "test-refresh" {
		t.Fatal("credential migration did not preserve authentication")
	}
}
