package auth

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Friendly CLI capabilities expand to API permissions here. Every capability
// includes application discovery and a renewable login.
var scopePermissions = map[string][]string{
	"apps":     {},
	"logs":     {"logs:read"},
	"deploy":   {"goals:read", "goals:write", "apps:deploy", "logs:read"},
	"network":  {"networks:connect", "networks:manage"},
	"database": {"networks:connect", "networks:manage", "databases:credentials:read"},
}

// ScopeNames returns the supported friendly names for CLI help and errors.
func ScopeNames() []string {
	names := make([]string, 0, len(scopePermissions))
	for name := range scopePermissions {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func loginScopes(requested []string, super bool) ([]string, error) {
	if super {
		if len(requested) != 0 {
			return nil, errors.New("choose either --super or --scope, not both")
		}
		return nil, nil // Filled from the server's advertised scopes after discovery.
	}
	if len(requested) == 0 {
		return nil, errors.New("use infra auth login --super (recommended) or --scope with a comma-separated list of capabilities")
	}
	scopes := []string{"apps:read", "offline_access"}
	for _, value := range requested {
		name := strings.TrimSpace(value)
		permissions, ok := scopePermissions[name]
		if !ok {
			return nil, fmt.Errorf("unknown --scope option %q; choose from: %s", name, strings.Join(ScopeNames(), ", "))
		}
		scopes = append(scopes, permissions...)
	}
	return normalizeOAuthScopes(scopes)
}

func normalizeOAuthScopes(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New("OAuth scopes must not be empty")
	}
	scopes := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		scope := strings.TrimSpace(value)
		if scope == "" {
			return nil, errors.New("OAuth scope names must not be empty")
		}
		// OAuth scope-token syntax (RFC 6749 section 3.3). Commas separate CLI values.
		for _, char := range scope {
			if char < '!' || char > '~' || char == '"' || char == '\\' || char == ',' {
				return nil, fmt.Errorf("invalid OAuth scope %q", scope)
			}
		}
		if !seen[scope] {
			scopes = append(scopes, scope)
			seen[scope] = true
		}
	}
	return scopes, nil
}
