package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/InfrastryAI/infra/internal/api"
)

const loadedResolver = `DNS configuration

resolver #9
  domain   : app.team.internal
  nameserver[0] : fd42:f1a5:6e74:3::53
  flags    : Request A records, Request AAAA records
  reach    : 0x00000002 (Reachable)
  order    : 1
`

func TestNativeResolverRequiresMatchingUnscopedIPv6Configuration(t *testing.T) {
	for name, configuration := range map[string]string{
		"not loaded":       "No DNS configuration available",
		"different domain": strings.ReplaceAll(loadedResolver, "app.team.internal", "other.team.internal"),
		"different server": strings.ReplaceAll(loadedResolver, dnsAddress, "::1"),
		"IPv4 only":        strings.ReplaceAll(loadedResolver, ", Request AAAA records", ""),
		"interface only":   strings.ReplaceAll(loadedResolver, "flags    :", "flags    : Scoped,"),
		"extra server":     loadedResolver + "  nameserver[1] : ::1\n",
		"separate blocks":  "resolver #1\n  domain : app.team.internal\n\nresolver #2\n  nameserver[0] : " + dnsAddress + "\n  flags : Request AAAA records\n",
	} {
		t.Run(name, func(t *testing.T) {
			if nativeResolverLoaded(configuration, "app.team.internal", dnsAddress) {
				t.Fatal("accepted a resolver that cannot handle this application's normal IPv6 lookups")
			}
		})
	}
	if !nativeResolverLoaded(loadedResolver, "app.team.internal", dnsAddress) {
		t.Fatal("did not recognize the application resolver")
	}
}

func TestNativeAddressesMustAllMatchAuthorizedService(t *testing.T) {
	const expected = "fd42:f1a5:6e74:1::1"
	for name, output := range map[string]string{
		"empty":       "",
		"no address":  "name: api.app.team.internal\n",
		"wrong IPv6":  "ipv6_address: fd42:f1a5:6e74:1::2\n",
		"IPv4 answer": "ip_address: 192.168.1.2\n",
		"mixed":       "ipv6_address: " + expected + "\nip_address: 192.168.1.2\n",
	} {
		t.Run(name, func(t *testing.T) {
			if nativeAddressMatches(output, expected) {
				t.Fatal("accepted an absent or unauthorized address")
			}
		})
	}
	if !nativeAddressMatches("name: api.app.team.internal\nipv6_address: fd42:f1a5:6e74:1:0:0:0:1\n", expected) {
		t.Fatal("rejected the authorized address in expanded form")
	}
}

func nativeDNSSession() api.NetworkSession {
	return api.NetworkSession{DNSSuffix: "app.team.internal", DNS: dnsAddress, Services: []api.NetworkService{
		{Hostname: "api.app.team.internal", Address: "fd42:f1a5:6e74:1::1"},
		{Hostname: "primary.app.team.internal", Address: "fd42:f1a5:6e74:1::2"},
	}}
}

func TestNativeReadinessWaitsForResolverAndEveryService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inspections, lookups := 0, 0
		var resolved []string
		session := nativeDNSSession()
		command := func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("DNS check has no deadline")
			}
			if binary == "/usr/sbin/scutil" {
				inspections++
				if inspections == 1 {
					return []byte("No DNS configuration available"), nil
				}
				return []byte(loadedResolver), nil
			}
			if inspections < 2 {
				t.Fatal("queried a private name before macOS loaded its resolver")
			}
			if binary != "/usr/bin/dscacheutil" {
				t.Fatalf("unexpected DNS command: %s", binary)
			}
			lookups++
			if lookups == 1 {
				return nil, errors.New("resolver has not received the update yet")
			}
			for _, service := range session.Services {
				if args[len(args)-1] == service.Hostname {
					resolved = append(resolved, service.Hostname)
					return []byte("ipv6_address: " + service.Address + "\n"), nil
				}
			}
			t.Fatal("queried an undeclared service")
			return nil, nil
		}
		if err := waitForNativeDNS(context.Background(), session, command); err != nil {
			t.Fatal(err)
		}
		if inspections != 3 || strings.Join(resolved, ",") != "api.app.team.internal,primary.app.team.internal" {
			t.Fatalf("reported ready before all names resolved: inspections=%d resolved=%v", inspections, resolved)
		}
	})
}

func TestNativeReadinessTimesOutWithoutDeclaringSuccess(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			command := func(_ context.Context, binary string, _ ...string) ([]byte, error) {
				if binary == "/usr/sbin/scutil" && loaded {
					return []byte(loadedResolver), nil
				}
				if binary == "/usr/bin/dscacheutil" && !loaded {
					t.Fatal("private name leaked to the default resolver")
				}
				return nil, nil
			}
			if err := waitForNativeDNS(context.Background(), nativeDNSSession(), command); err == nil {
				t.Fatal("reported ready with unavailable private DNS")
			}
		})
	}
}

func TestNativeReadinessCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("ran DNS check after cancellation")
		return nil, nil
	}
	if err := waitForNativeDNS(ctx, nativeDNSSession(), command); err == nil {
		t.Fatal("cancelled setup reported ready")
	}
}
