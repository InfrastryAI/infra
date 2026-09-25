package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseDumpNeverOverwritesAnExistingPath(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			target := filepath.Join(directory, "existing.sql")
			if err := os.WriteFile(target, []byte("keep this backup"), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(directory, "backup.sql")
			switch kind {
			case "file":
				output = target
			case "symlink":
				if err := os.Symlink(target, output); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(output, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			command := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
			err := command.runDatabaseDump(t.Context(), "acme/customer-api", proxyOptions{database: true, connect: true}, output)
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("existing path error = %v", err)
			}
			content, err := os.ReadFile(target)
			if err != nil || string(content) != "keep this backup" {
				t.Fatalf("existing backup changed: %q, %v", content, err)
			}
			if _, err := os.Lstat(output); err != nil {
				t.Fatalf("existing output removed: %v", err)
			}
		})
	}
}

func TestDatabaseDumpRejectsInvalidOutputBeforeConnecting(t *testing.T) {
	command := New(Dependencies{})
	output := filepath.Join(t.TempDir(), "missing", "backup.sql")
	err := command.runDatabaseDump(t.Context(), "acme/customer-api", proxyOptions{database: true, connect: true}, output)
	if err == nil || !strings.Contains(err.Error(), "cannot create dump file") {
		t.Fatalf("invalid output error = %v", err)
	}
}

func TestDatabaseDumpKeepsConnectionsOnTheProxy(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			options := proxyOptions{engine: engine, user: "operator", databaseName: "app", dumpOutput: io.Discard}
			binary, args, err := databaseClient(options, "127.0.0.1:15432")
			if err != nil {
				t.Fatal(err)
			}
			if engine == "postgres" {
				if binary != "pg_dump" || !contains(args, "127.0.0.1") || !contains(args, "15432") || !contains(args, "--format=plain") || contains(args, "-X") {
					t.Fatalf("unexpected PostgreSQL dump client: %s %v", binary, args)
				}
			} else if binary != "mysqldump" || !contains(args, "--host=127.0.0.1") || !contains(args, "--port=15432") || !contains(args, "--single-transaction") || !contains(args, "--no-tablespaces") || !contains(args, "--set-gtid-purged=OFF") || args[len(args)-1] != "app" {
				t.Fatalf("unexpected MySQL dump client: %s %v", binary, args)
			}
			for _, name := range []string{"", "host=other", "postgres://other/app", "--all-databases"} {
				options.databaseName = name
				if _, _, err := databaseClient(options, "127.0.0.1:15432"); err == nil {
					t.Fatalf("accepted connection override %q", name)
				}
			}
		})
	}
}
