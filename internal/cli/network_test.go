package cli

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/InfrastryAI/infra/internal/api"
)

func TestPrivateConnectionsRequireExplicitTeam(t *testing.T) {
	for _, ref := range []string{"app", "/app", "team/", "team/app/other", "team/../app", "https://example.com"} {
		if appReference.MatchString(ref) {
			t.Fatalf("ambiguous reference accepted: %s", ref)
		}
	}
	if !appReference.MatchString("team/app") {
		t.Fatal("valid reference rejected")
	}
}

func TestDatabaseClientUsesSelectedServicesConfiguration(t *testing.T) {
	var session api.NetworkSession
	if err := json.Unmarshal([]byte(`{"services":[
		{"name":"other","kind":"database","connection":{"username":"other_user","database":"other_db"}},
		{"name":"primary","kind":"database","connection":{"username":"app_user","database":"app_db"}}
	]}`), &session); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, engine, user, database, wantUser, wantDatabase string
	}{
		{"postgres defaults", "postgres", "", "", "app_user", "app_db"},
		{"mysql defaults", "mysql", "", "", "app_user", "app_db"},
		{"user override", "postgres", "operator", "", "operator", "app_db"},
		{"database override", "postgres", "", "analytics", "app_user", "analytics"},
		{"both overrides", "postgres", "operator", "analytics", "operator", "analytics"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := databaseOptions(session, proxyOptions{service: "primary", engine: test.engine, user: test.user, databaseName: test.database})
			binary, args, err := databaseClient(options, "127.0.0.1:15432")
			wantBinary := "psql"
			wantArgs := []string{"-X", "-W", "-h", "127.0.0.1", "-p", "15432", "-U", test.wantUser, "-d", test.wantDatabase}
			if test.engine == "mysql" {
				wantBinary = "mysql"
				wantArgs = []string{"--protocol=TCP", "--host=127.0.0.1", "--port=15432", "--user=" + test.wantUser, "--database=" + test.wantDatabase, "--password"}
			}
			if err != nil || binary != wantBinary || !reflect.DeepEqual(args, wantArgs) {
				t.Fatalf("database client = %s %v %v, want %s %v", binary, args, err, wantBinary, wantArgs)
			}
		})
	}
}

func TestDatabaseClientRejectsMissingOrUnsafeConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, service, kind, user, database string
	}{
		{"missing user", "primary", "database", "", "app"},
		{"missing database", "primary", "database", "operator", ""},
		{"other service", "other", "database", "operator", "app"},
		{"non database", "primary", "web", "operator", "app"},
		{"connection string", "primary", "database", "operator", "host=other"},
		{"connection URI", "primary", "database", "operator", "postgres://other/app"},
		{"user option", "primary", "database", "-operator", "app"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := api.NetworkService{Name: test.service, Kind: test.kind}
			service.Connection.Username = test.user
			service.Connection.Database = test.database
			session := api.NetworkSession{Services: []api.NetworkService{service}}
			options := databaseOptions(session, proxyOptions{service: "primary", engine: "postgres"})
			if _, _, err := databaseClient(options, "127.0.0.1:15432"); err == nil {
				t.Fatal("accepted missing or unsafe configuration")
			}
		})
	}
}
func TestProxyOnlySelectsDeclaredPorts(t *testing.T) {
	session := api.NetworkSession{Services: []api.NetworkService{{Name: "api", Kind: "web", Address: "fd42:f1a5:6e74:1::1"}}}
	session.Services[0].Ports = append(session.Services[0].Ports, struct {
		Protocol string `json:"protocol"`
		Port     uint16 `json:"port"`
	}{Protocol: "tcp", Port: 8080})
	if _, err := proxyDestination(session, proxyOptions{service: "api", remotePort: 5432}); err == nil {
		t.Fatal("undeclared port accepted")
	}
	if _, err := proxyDestination(session, proxyOptions{service: "other"}); err == nil {
		t.Fatal("other service accepted")
	}
	if _, err := proxyDestination(session, proxyOptions{service: "api", database: true}); err == nil {
		t.Fatal("non-database accepted")
	}
	if target, err := proxyDestination(session, proxyOptions{service: "api"}); err != nil || target != "[fd42:f1a5:6e74:1::1]:8080" {
		t.Fatalf("declared port: %s %v", target, err)
	}
}

func TestDatabaseClientKeepsConnectionsOnTheProxy(t *testing.T) {
	options := proxyOptions{engine: "postgres", user: "operator", databaseName: "app"}
	binary, args, err := databaseClient(options, "127.0.0.1:15432")
	if err != nil || binary != "psql" || !contains(args, "-W") || !contains(args, "127.0.0.1") {
		t.Fatalf("unexpected database client: %s %v %v", binary, args, err)
	}
	for _, name := range []string{"host=other", "postgres://other/app", "-other"} {
		options.databaseName = name
		if _, _, err := databaseClient(options, "127.0.0.1:15432"); err == nil {
			t.Fatalf("accepted connection override %q", name)
		}
	}
	options.databaseName, options.engine = "app", "mysql"
	binary, args, err = databaseClient(options, "127.0.0.1:15432")
	if err != nil || binary != "mysql" || !contains(args, "--password") || !contains(args, "--host=127.0.0.1") {
		t.Fatalf("unexpected mysql client: %s %v %v", binary, args, err)
	}
}
