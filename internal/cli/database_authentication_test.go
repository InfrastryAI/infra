package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseCredentialFilesArePrivateAndEscaped(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			options := proxyOptions{engine: engine, user: `user:name`, databaseName: `db\name`}
			password := `a:b\c"d#e`
			args, env, cleanup, err := databaseAuthentication(options, "127.0.0.1:5432", password)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			path := ""
			for _, entry := range env {
				if strings.HasPrefix(entry, "PGPASSFILE=") {
					path = strings.TrimPrefix(entry, "PGPASSFILE=")
				}
			}
			if engine == "mysql" {
				path = strings.TrimPrefix(args[0], "--defaults-file=")
			}
			for file, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
				stat, err := os.Stat(file)
				if err != nil || stat.Mode().Perm() != mode {
					t.Fatalf("unsafe credential file permissions: %v", err)
				}
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := "127.0.0.1:5432:db\\\\name:user\\:name:a\\:b\\\\c\"d#e\n"
			if engine == "mysql" {
				expected = "[client]\npassword=\"a:b\\\\c\\\"d#e\"\n"
			}
			if string(content) != expected {
				t.Fatalf("incorrectly escaped credential file: %q", content)
			}
			if strings.Contains(strings.Join(args, " ")+strings.Join(env, " "), password) {
				t.Fatal("password in process arguments or environment")
			}
		})
	}
}

func TestDatabaseCredentialFilesRejectLineInjection(t *testing.T) {
	for _, password := range []string{"", "one\ntwo", "one\rtwo", "one\x00two"} {
		_, _, cleanup, err := databaseAuthentication(proxyOptions{engine: "postgres", user: "user", databaseName: "db"}, "127.0.0.1:5432", password)
		if err == nil || cleanup != nil {
			t.Fatal("unsafe password accepted")
		}
	}
}
