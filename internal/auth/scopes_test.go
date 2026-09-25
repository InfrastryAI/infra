package auth

import (
	"slices"
	"testing"
)

func TestLoginScopeSelection(t *testing.T) {
	for _, test := range []struct {
		name    string
		scopes  []string
		super   bool
		want    []string
		invalid bool
	}{
		{name: "apps discovery and renewal", scopes: []string{"apps"}, want: []string{"apps:read", "offline_access"}},
		{name: "logs includes discovery", scopes: []string{"logs"}, want: []string{"apps:read", "offline_access", "logs:read"}},
		{name: "deploy includes Git uploads, goals and logs", scopes: []string{"deploy"}, want: []string{"apps:read", "offline_access", "goals:read", "goals:write", "apps:deploy", "logs:read"}},
		{name: "network excludes database credentials", scopes: []string{"network"}, want: []string{"apps:read", "offline_access", "networks:connect", "networks:manage"}},
		{name: "database includes network", scopes: []string{"database"}, want: []string{"apps:read", "offline_access", "networks:connect", "networks:manage", "databases:credentials:read"}},
		{name: "combined capabilities deduplicate", scopes: []string{" network ", "database", "logs", "network"}, want: []string{"apps:read", "offline_access", "networks:connect", "networks:manage", "databases:credentials:read", "logs:read"}},
		{name: "super deferred to discovery", super: true},
		{name: "selection required", invalid: true},
		{name: "exclusive flags", super: true, scopes: []string{"apps"}, invalid: true},
		{name: "empty scope", scopes: []string{"apps", ""}, invalid: true},
		{name: "space separator", scopes: []string{"network database"}, invalid: true},
		{name: "raw API scope rejected", scopes: []string{"apps:read"}, invalid: true},
		{name: "unknown capability", scopes: []string{"admin"}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := loginScopes(test.scopes, test.super)
			if (err != nil) != test.invalid || !slices.Equal(got, test.want) {
				t.Fatalf("loginScopes() = %v, %v; want %v, invalid %t", got, err, test.want, test.invalid)
			}
		})
	}
}
