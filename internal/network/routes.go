package network

import (
	"net/netip"
	"strings"
)

// The macOS numeric routing table's first column is a destination or prefix.
// Default routes are expected; any more specific overlap belongs to another
// connection and must not be silently overridden by our host routes.
func routeConflicts(table string, addresses []string) bool {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		destination := fields[0]
		prefix, err := netip.ParsePrefix(destination)
		if err != nil {
			ip, parseErr := netip.ParseAddr(destination)
			if parseErr != nil {
				continue
			}
			prefix = netip.PrefixFrom(ip, ip.BitLen())
		}
		if prefix.Bits() == 0 {
			continue
		}
		for _, address := range addresses {
			ip, err := netip.ParseAddr(address)
			if err == nil && prefix.Contains(ip) {
				return true
			}
		}
	}
	return false
}
