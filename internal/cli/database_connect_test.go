package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfrastryAI/infra/internal/config"
)

func TestDatabaseConnectFetchesCredentialsAndCleansUp(t *testing.T) {
	for _, test := range []struct {
		name, engine, script                         string
		missingClient, missingScope, failCredentials bool
		omitDatabase, extraDatabase, noDatabase      bool
		proxy, passwordPrompt, dump, defaultOutput   bool
		service, databaseName, wantError             string
		wantCode                                     int
	}{
		{name: "missing client", engine: "postgres", missingClient: true, wantCode: 1},
		{name: "postgres", engine: "postgres"},
		{name: "mysql", engine: "mysql"},
		{name: "client failure", engine: "postgres", script: "exit 7", wantCode: 1},
		{name: "canceled client", engine: "postgres", script: "printf CLIENT_READY; exec /bin/sleep 30"},
		{name: "missing consent", engine: "postgres", missingScope: true, wantCode: 1},
		{name: "credential failure", engine: "postgres", failCredentials: true, wantCode: 1},
		{name: "infer postgres", engine: "postgres", omitDatabase: true},
		{name: "infer mysql", engine: "mysql", omitDatabase: true},
		{name: "infer password prompt", engine: "postgres", omitDatabase: true, passwordPrompt: true, missingScope: true},
		{name: "infer proxy", omitDatabase: true, proxy: true},
		{name: "multiple databases", engine: "postgres", omitDatabase: true, extraDatabase: true, wantCode: 2, wantError: "select one with --database: primary, analytics"},
		{name: "multiple databases proxy", omitDatabase: true, extraDatabase: true, proxy: true, wantCode: 2, wantError: "select one with --database: primary, analytics"},
		{name: "explicit database among several", engine: "postgres", extraDatabase: true, service: "analytics"},
		{name: "no database", engine: "postgres", omitDatabase: true, noDatabase: true, wantCode: 1, wantError: "no database services are available"},
		{name: "no database proxy", omitDatabase: true, noDatabase: true, proxy: true, wantCode: 1, wantError: "no database services are available"},
		{name: "unknown explicit database", engine: "postgres", service: "missing", wantCode: 1, wantError: "the selected service is not available"},
		{name: "explicit web service", engine: "postgres", service: "api", wantCode: 1, wantError: "the selected service is not a database"},
		{name: "dump postgres with default filename", engine: "postgres", dump: true, omitDatabase: true, defaultOutput: true},
		{name: "dump mysql", engine: "mysql", dump: true, omitDatabase: true},
		{name: "dump different database", engine: "mysql", dump: true, databaseName: "reporting"},
		{name: "dump client failure", engine: "postgres", dump: true, script: "exit 7", wantCode: 1},
		{name: "dump canceled client", engine: "postgres", dump: true, script: "printf CLIENT_READY >&2; exec /bin/sleep 30", wantCode: 1},
		{name: "dump missing client", engine: "postgres", dump: true, missingClient: true, wantCode: 1},
		{name: "dump missing consent", engine: "postgres", dump: true, missingScope: true, wantCode: 1},
		{name: "dump credential failure", engine: "postgres", dump: true, failCredentials: true, wantCode: 1},
		{name: "dump postgres password prompt", engine: "postgres", dump: true, passwordPrompt: true, missingScope: true},
		{name: "dump mysql password prompt", engine: "mysql", dump: true, passwordPrompt: true, missingScope: true},
		{name: "dump multiple databases", engine: "postgres", dump: true, omitDatabase: true, extraDatabase: true, wantCode: 2, wantError: "select one with --database: primary, analytics"},
		{name: "dump explicit database among several", engine: "postgres", dump: true, extraDatabase: true, service: "analytics"},
		{name: "dump no database", engine: "postgres", dump: true, omitDatabase: true, noDatabase: true, wantCode: 1, wantError: "no database services are available"},
		{name: "dump explicit web service", engine: "postgres", dump: true, service: "api", wantCode: 1, wantError: "the selected service is not a database"},
	} {
		t.Run(test.name, func(t *testing.T) {

			// A short path keeps the Unix control socket within macOS's path limit.
			directory, err := os.MkdirTemp("", "infra-db-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(directory) })
			t.Setenv("PATH", directory)
			t.Setenv("INFRA_TEST_RECORD", directory)
			t.Setenv("PGPASSWORD", "inherited-password")
			t.Setenv("PGHOSTADDR", "203.0.113.1")
			t.Setenv("MYSQL_PWD", "inherited-password")
			binary := "psql"
			if test.engine == "mysql" {
				binary = "mysql"
			}
			if test.dump {
				binary = "pg_dump"
				if test.engine == "mysql" {
					binary = "mysqldump"
				}
			}
			if !test.missingClient {
				script := `#!/bin/sh
credential="$PGPASSFILE"
case "$1" in --defaults-file=*) credential="${1#--defaults-file=}" ;; esac
printf '%s' "$credential" > "$INFRA_TEST_RECORD/path"
printf '%s\n' "$@" > "$INFRA_TEST_RECORD/args"
printf '%s%s%s' "$PGPASSWORD" "$PGHOSTADDR" "$MYSQL_PWD" > "$INFRA_TEST_RECORD/inherited"
if [ -n "$credential" ]; then /bin/cat "$credential" > "$INFRA_TEST_RECORD/content"; fi
`
				if test.dump {
					script += "printf '%s\\n' 'CREATE TABLE example (id integer);' 'INSERT INTO example VALUES (1);'\n"
				}
				script += test.script + "\n"
				if err := os.WriteFile(filepath.Join(directory, binary), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}

			now := time.Now()
			key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
			services := []map[string]any{{
				"id": "web", "name": "api", "kind": "web", "address": "fd42:f1a5:6e74:1::2",
				"hostname": "api.customer-api.acme.internal",
				"ports":    []map[string]any{{"protocol": "tcp", "port": 8080}},
			}}
			if !test.noDatabase {
				services = append(services, map[string]any{
					"id": "service", "name": "primary", "kind": "database", "address": "fd42:f1a5:6e74:1::1",
					"hostname":   "primary.customer-api.acme.internal",
					"ports":      []map[string]any{{"protocol": "tcp", "port": 5432}},
					"connection": map[string]string{"engine": test.engine, "username": "app_user", "database": "app_db"},
				})
			}
			if test.extraDatabase {
				services = append(services, map[string]any{
					"id": "analytics-service", "name": "analytics", "kind": "database", "address": "fd42:f1a5:6e74:1::3",
					"hostname":   "analytics.customer-api.acme.internal",
					"ports":      []map[string]any{{"protocol": "tcp", "port": 5432}},
					"connection": map[string]string{"engine": test.engine, "username": "app_user", "database": "app_db"},
				})
			}
			credentialService := "service"
			if test.service == "analytics" {
				credentialService = "analytics-service"
			}
			var connected, disconnected, fetched atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer access" {
					t.Errorf("authorization = %q", request.Header.Get("Authorization"))
				}
				switch request.Method + " " + request.URL.Path {
				case "POST /api/v1/network-devices":
					json.NewEncoder(writer).Encode(map[string]any{"device": map[string]string{"id": "device"}})
				case "POST /api/v1/teams/acme/apps/customer-api/network-sessions":
					connected.Store(true)
					json.NewEncoder(writer).Encode(map[string]any{"session": map[string]any{
						"id": "session", "app_ref": "acme/customer-api", "mode": "proxy",
						"source": "fd42:f1a5:6e74:2::1", "dns": "fd42:f1a5:6e74:3::53",
						"dns_suffix": "customer-api.acme.internal", "mtu": 1280, "expires_at": now.Add(time.Hour),
						"gateway":  map[string]any{"endpoint": "127.0.0.1:51820", "public_key": key},
						"services": services,
					}})
				case "POST /api/v1/network-sessions/session/services/" + credentialService + "/credentials":
					fetched.Store(true)
					if test.failCredentials {
						writer.WriteHeader(403)
						writer.Write([]byte(`{"error":{"message":"not allowed"}}`))
						return
					}
					json.NewEncoder(writer).Encode(map[string]any{"credentials": map[string]string{"engine": test.engine, "username": "app_user", "database": "app_db", "password": "temporary-test-secret"}})
				case "DELETE /api/v1/network-sessions/session":
					disconnected.Store(true)
					writer.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			configurationPath := filepath.Join(directory, "config.json")
			configuration := config.Config{}
			scopes := []string{"networks:connect", "networks:manage"}
			if !test.missingScope {
				scopes = append(scopes, "databases:credentials:read")
			}
			configuration.SetProfile(config.Profile{
				APIURL: server.URL, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
				Scopes: scopes,
			})
			if err := (config.Store{Path: configurationPath}).Save(configuration); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stdout := &databaseOutput{cancel: cancel, cancelOnForward: test.proxy}
			stderr := &databaseOutput{cancel: cancel}
			command := New(Dependencies{
				Stdout: stdout, Stderr: stderr, HTTPClient: server.Client(),
				Credentials: &config.MemoryCredentialStore{}, Getenv: func(string) string { return "" },
				NetworkKey: func(string, string, string) (string, string, error) { return key, key, nil },
				WorkingDir: func() (string, error) { return directory, nil }, Now: func() time.Time { return now },
			})
			action := "connect"
			if test.proxy {
				action = "proxy"
			}
			if test.dump {
				action = "dump"
			}
			arguments := []string{
				"--api-url", server.URL, "--config", configurationPath,
				"db", action, "acme/customer-api",
			}
			if !test.omitDatabase {
				service := test.service
				if service == "" {
					service = "primary"
				}
				arguments = append(arguments, "--database", service)
			}
			if test.passwordPrompt {
				arguments = append(arguments, "--password-prompt")
			}
			if test.databaseName != "" {
				arguments = append(arguments, "--database-name", test.databaseName)
			}
			outputPath := filepath.Join(directory, "backup with spaces.sql")
			if test.dump {
				if test.defaultOutput {
					outputPath = filepath.Join(directory, "acme-customer-api-"+now.UTC().Format("20060102T150405.000000000Z")+".sql")
				} else {
					arguments = append(arguments, "-o", "backup with spaces.sql")
				}
			}
			code := command.Run(ctx, arguments)

			if code != test.wantCode {
				t.Fatalf("Run() = %d: %s", code, stderr.String())
			}
			if test.dump {
				if test.wantCode == 0 {
					content, err := os.ReadFile(outputPath)
					if err != nil || string(content) != "CREATE TABLE example (id integer);\nINSERT INTO example VALUES (1);\n" {
						t.Fatalf("dump contents = %q, error = %v", content, err)
					}
					stat, err := os.Stat(outputPath)
					if err != nil || stat.Mode().Perm() != 0600 {
						t.Fatalf("unsafe dump permissions: %v", err)
					}
					if !strings.Contains(stdout.String(), "Database dump saved to "+outputPath) || strings.Contains(stdout.String(), "CREATE TABLE") {
						t.Fatalf("dump status = %q", stdout.String())
					}
					args, err := os.ReadFile(filepath.Join(directory, "args"))
					if err != nil || !strings.Contains(string(args), defaultString(test.databaseName, "app_db")+"\n") {
						t.Fatalf("dump omitted database name: %s, %v", args, err)
					}
				} else {
					if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
						t.Fatalf("incomplete dump was not removed: %v", err)
					}
					if strings.Contains(stdout.String(), "Database dump saved") {
						t.Fatal("failed dump reported success")
					}
				}
			}
			if test.missingScope && !test.passwordPrompt {
				if connected.Load() || fetched.Load() || !strings.Contains(stderr.String(), "infra auth login --super") {
					t.Fatalf("missing consent: %s", stderr.String())
				}
				return
			}
			if !connected.Load() || !disconnected.Load() {
				t.Fatal("session was not connected and cleaned up")
			}
			if test.wantError != "" {
				if fetched.Load() || !strings.Contains(stderr.String(), test.wantError) {
					t.Fatalf("database selection: %s", stderr.String())
				}
			} else if test.proxy {
				if fetched.Load() || !strings.Contains(stdout.String(), "to [fd42:f1a5:6e74:1::1]:5432") {
					t.Fatalf("database proxy: %s", stdout.String())
				}
			} else if test.passwordPrompt {
				args, err := os.ReadFile(filepath.Join(directory, "args"))
				prompt := "-W\n"
				if test.engine == "mysql" {
					prompt = "--password\n"
				}
				if fetched.Load() || err != nil || !strings.Contains(string(args), prompt) || !strings.Contains(string(args), "app_user\n") || !strings.Contains(string(args), "app_db\n") {
					t.Fatalf("password prompt: args %s, error %v", args, err)
				}
			} else if test.missingClient {
				if fetched.Load() || !strings.Contains(stderr.String(), "install "+binary) {
					t.Fatalf("missing-client handling: %s", stderr.String())
				}
			} else {
				if !fetched.Load() {
					t.Fatal("credentials not fetched")
				}
				if !test.failCredentials {
					path, err := os.ReadFile(filepath.Join(directory, "path"))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(string(path)); !os.IsNotExist(err) {
						t.Fatal("credential file not removed")
					}
					if _, err := os.Stat(filepath.Dir(string(path))); !os.IsNotExist(err) {
						t.Fatal("credential directory not removed")
					}
					content, err := os.ReadFile(filepath.Join(directory, "content"))
					if err != nil || !strings.Contains(string(content), "temporary-test-secret") {
						t.Fatalf("client did not receive password: %v", err)
					}
					args, _ := os.ReadFile(filepath.Join(directory, "args"))
					if strings.Contains(string(args), "temporary-test-secret") || strings.Contains(string(args), "--password") || strings.Contains(string(args), "-W") {
						t.Fatal("password or prompt in client arguments")
					}
					inherited, _ := os.ReadFile(filepath.Join(directory, "inherited"))
					if len(inherited) != 0 {
						t.Fatal("inherited database overrides")
					}
				}
			}
			if strings.Contains(stdout.String()+stderr.String(), "temporary-test-secret") {
				t.Fatal("password leaked to output")
			}
			if _, err := os.Stat(command.networkSocket()); !os.IsNotExist(err) {
				t.Fatalf("control socket was not removed: %v", err)
			}
		})
	}
}

type databaseOutput struct {
	mu              sync.Mutex
	buffer          bytes.Buffer
	cancel          context.CancelFunc
	cancelOnForward bool
}

func (out *databaseOutput) Write(data []byte) (int, error) {
	out.mu.Lock()
	defer out.mu.Unlock()
	if strings.Contains(string(data), "CLIENT_READY") || (out.cancelOnForward && strings.Contains(string(data), "Forwarding ")) {
		out.cancel()
	}
	return out.buffer.Write(data)
}
func (out *databaseOutput) String() string {
	out.mu.Lock()
	defer out.mu.Unlock()
	return fmt.Sprint(out.buffer.String())
}
