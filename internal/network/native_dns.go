package network

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	"github.com/InfrastryAI/infra/internal/api"
)

type nativeDNSCommand func(context.Context, string, ...string) ([]byte, error)

func nativeDNSOutput(ctx context.Context, binary string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, binary, args...).Output()
}

// configd loads /etc/resolver through asynchronous filesystem notifications.
// Wait for the scoped configuration before querying names, so the first lookup
// does not go to the default resolver and cache a negative answer. Then use the
// macOS system resolver (also used by curl/psql), independently of Go DNS flags.
func waitForNativeDNS(ctx context.Context, session api.NetworkSession, command nativeDNSCommand) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	resolverLoaded := false
	for ctx.Err() == nil {
		attempt, stop := context.WithTimeout(ctx, 2*time.Second)
		configuration, err := command(attempt, "/usr/sbin/scutil", "--dns")
		stop()
		resolverLoaded = err == nil && nativeResolverLoaded(string(configuration), session.DNSSuffix, session.DNS)
		if resolverLoaded {
			resolved := true
			for _, service := range session.Services {
				attempt, stop := context.WithTimeout(ctx, 2*time.Second)
				output, err := command(attempt, "/usr/bin/dscacheutil", "-q", "host", "-a", "name", service.Hostname)
				stop()
				if err != nil || !nativeAddressMatches(string(output), service.Address) {
					resolved = false
					break
				}
			}
			if resolved && ctx.Err() == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	if resolverLoaded {
		return errors.New("private names could not be resolved; check your network connection and try again")
	}
	return errors.New("private DNS could not be activated on this Mac; disconnect and try again")
}

func nativeResolverLoaded(configuration, suffix, server string) bool {
	expected, err := netip.ParseAddr(server)
	if err != nil {
		return false
	}
	for _, block := range strings.Split(configuration, "\n\n") {
		domain, flags := "", ""
		servers, matching := 0, 0
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			switch {
			case key == "domain":
				domain = value
			case key == "flags":
				flags = value
			case strings.HasPrefix(key, "nameserver["):
				servers++
				if address, err := netip.ParseAddr(value); err == nil && address == expected {
					matching++
				}
			}
		}
		if domain == suffix && servers == 1 && matching == 1 &&
			strings.Contains(flags, "Request AAAA records") && !strings.Contains(flags, "Scoped") {
			return true
		}
	}
	return false
}

func nativeAddressMatches(output, expected string) bool {
	address, err := netip.ParseAddr(expected)
	if err != nil {
		return false
	}
	found := false
	for _, line := range strings.Split(output, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		if key != "ip_address" && key != "ipv6_address" {
			continue
		}
		resolved, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil || resolved != address {
			return false
		}
		found = true
	}
	return found
}
