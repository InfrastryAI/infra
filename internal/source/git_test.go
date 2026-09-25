package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitializeCreatesCleanRepository(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repository, err := (Git{}).Initialize(t.Context(), directory, "main", "origin")
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if repository.Root != directory || repository.Branch != "main" || len(repository.Revision) != 40 || repository.Dirty {
		t.Fatalf("Initialize() = %#v", repository)
	}
	message := gitOutput(t, directory, "log", "-1", "--format=%s")
	if strings.TrimSpace(message) != "Initial deployment" {
		t.Fatalf("commit message = %q", message)
	}
}

func TestInitializeRefusesSensitiveFiles(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("SECRET=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (Git{}).Initialize(t.Context(), directory, "main", "origin")
	if err == nil || !strings.Contains(err.Error(), ".env") {
		t.Fatalf("Initialize() error = %v", err)
	}
	if output := gitOutput(t, directory, "rev-list", "--all", "--count"); strings.TrimSpace(output) != "0" {
		t.Fatalf("commit count = %q", output)
	}
	if output := gitOutput(t, directory, "ls-files"); strings.TrimSpace(output) != "" {
		t.Fatalf("sensitive files were staged: %q", output)
	}
}

func TestInspectFindsDirtyTreeAndRemotes(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := Git{}
	if _, err := git.Initialize(t.Context(), directory, "trunk", "origin"); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "remote", "add", "origin", "git@github.com:acme/example.git")
	runGit(t, directory, "remote", "add", "infrastry", "https://git.infrastry.test/acme/example.git")
	if err := os.WriteFile(filepath.Join(directory, "new.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repository, err := git.Inspect(t.Context(), directory, "origin", "")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !repository.Dirty || repository.Branch != "trunk" {
		t.Fatalf("Inspect() = %#v", repository)
	}
	if repository.RemoteURL != "git@github.com:acme/example.git" || repository.HostedRepositoryURL != "https://git.infrastry.test/acme/example.git" {
		t.Fatalf("Inspect() remotes = %q / %q", repository.RemoteURL, repository.HostedRepositoryURL)
	}
}

func TestInspectUnbornRepositoryPreservesReviewDetails(t *testing.T) {
	directory := t.TempDir()
	runGit(t, directory, "init", "--initial-branch=trunk")
	runGit(t, directory, "remote", "add", "infrastry", "https://infrastry.test/git/acme/app.git")
	repository, err := (Git{}).Inspect(t.Context(), directory, "origin", "")
	if !errors.Is(err, ErrNoCommits) || repository.Root != directory || repository.Branch != "trunk" || repository.HostedRepositoryURL != "https://infrastry.test/git/acme/app.git" || repository.Revision != "" {
		t.Fatalf("unborn repository review: %+v, %v", repository, err)
	}
	if staged := gitOutput(t, directory, "ls-files"); staged != "" {
		t.Fatalf("inspection staged files: %s", staged)
	}
}

func TestPublicCloneURLRemovesCredentialsAndConvertsSSH(t *testing.T) {
	tests := map[string]string{
		"git@github.com:Acme/example.git":          "https://github.com/Acme/example.git",
		"ssh://git@gitlab.com/acme/example.git":    "https://gitlab.com/acme/example.git",
		"https://token@code.test/acme/example.git": "https://code.test/acme/example.git",
	}
	for input, expected := range tests {
		actual, ok := publicCloneURL(input)
		if !ok || actual != expected {
			t.Errorf("publicCloneURL(%q) = %q, %t; want %q, true", input, actual, ok, expected)
		}
	}
	if _, ok := publicCloneURL("file:///tmp/repository"); ok {
		t.Fatal("publicCloneURL(file) accepted local repository")
	}
}

func TestPublicRemoteUsesAnonymousSmartHTTPAdvertisement(t *testing.T) {
	revision := "0123456789abcdef0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/acme/example.git/info/refs" || request.URL.Query().Get("service") != "git-upload-pack" {
			t.Errorf("request URL = %s", request.URL.String())
		}
		if request.Header.Get("Authorization") != "" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = writer.Write([]byte(
			packet("# service=git-upload-pack\n") + "0000" +
				packet(revision+" HEAD\x00symref=HEAD:refs/heads/main\n") +
				packet(revision+" refs/heads/main\n") + "0000",
		))
	}))
	defer server.Close()

	git := Git{HTTPClient: server.Client()}
	repositoryURL, public := git.PublicRemote(t.Context(), Repository{
		RemoteURL: server.URL + "/acme/example.git", Branch: "main", Revision: revision,
	})
	if !public || repositoryURL != server.URL+"/acme/example.git" {
		t.Fatalf("PublicRemote() = %q, %t", repositoryURL, public)
	}

	_, public = git.PublicRemote(t.Context(), Repository{
		RemoteURL: server.URL + "/acme/example.git", Branch: "main", Revision: strings.Repeat("f", 40),
	})
	if public {
		t.Fatal("PublicRemote() accepted an unadvertised revision")
	}
}

func TestPushUploadsCommitWithScopedAuthorization(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := (Git{}).Initialize(t.Context(), directory, "main", "origin")
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent commit after inspection must not change what gets uploaded.
	runGit(t, directory, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "--no-gpg-sign", "-m", "Later commit")
	root := t.TempDir()
	bare := filepath.Join(root, "app.git")
	runGit(t, root, "init", "--bare", "--initial-branch=main", bare)
	runGit(t, bare, "config", "http.receivepack", "true")
	gitExecutable, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: gitExecutable, Args: []string{"http-backend"}, Root: "/git", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only" {
			t.Error("Git push did not supply its scoped authorization")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer server.Close()
	if err := (Git{}).Push(t.Context(), repository, server.URL+"/git/app.git", "Bearer test-only"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitOutput(t, bare, "rev-parse", "refs/heads/main")); got != repository.Revision {
		t.Fatalf("uploaded revision %s, want %s", got, repository.Revision)
	}
	configuration := gitOutput(t, directory, "config", "--local", "--list")
	if strings.Contains(configuration, "test-only") || strings.Contains(configuration, "extraheader") {
		t.Fatal("Git push persisted authorization")
	}

	// A local insteadOf rule must not redirect the API token to another host.
	leaked := make(chan bool, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("Authorization") != ""
		http.Error(w, "rejected", http.StatusForbidden)
	}))
	defer other.Close()
	runGit(t, directory, "config", "url."+other.URL+"/.insteadOf", server.URL+"/")
	if err := (Git{}).Push(t.Context(), repository, server.URL+"/git/app.git", "Bearer test-only"); err == nil {
		t.Fatal("rewritten push unexpectedly succeeded")
	}
	select {
	case secret := <-leaked:
		if secret {
			t.Fatal("Git URL rewrite leaked the API token")
		}
	default:
		t.Fatal("test did not exercise Git URL rewriting")
	}
}

func packet(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func gitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}
