package network

import "testing"

func TestRouteConflictsIncludesBroaderVPNRoutes(t *testing.T) {
	addresses := []string{"fd42:f1a5:6e74:1::1", dnsAddress}
	for _, route := range []string{"fd42:f1a5:6e74::/48", "fd42:f1a5:6e74:1::/64", "fd42:f1a5:6e74:1::1", dnsAddress} {
		if !routeConflicts(route+" link#9 Uc utun4", addresses) {
			t.Fatalf("accepted overlap %s", route)
		}
	}
	if routeConflicts("Destination Gateway Flags Netif\ndefault fe80::1 UG en0\n::/0 fe80::1 UG en0\nfe80::/64 link#1 Uc en0\nfd11::/64 link#9 Uc utun4", addresses) {
		t.Fatal("unrelated/default route conflicts")
	}
}
