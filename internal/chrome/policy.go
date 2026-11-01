package chrome

import (
	"net"
	"net/netip"
	"slices"
)

var blockedAddrs = []netip.Addr{
	netip.MustParseAddr("100.100.100.200"), // Alibaba metadata
	netip.MustParseAddr("168.63.129.16"),   // Azure wire server
	netip.MustParseAddr("192.0.0.192"),     // Oracle metadata
}

var blockedNets = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"; Linux routes 0.0.0.0 to localhost
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, incl. broadcast
	netip.MustParsePrefix("fd00:ec2::/32"),  // AWS IMDS over IPv6
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2001::/32"),      // Teredo embeds an obfuscated IPv4
}

// Blocked reports whether the browser must not connect to ip:port.
//
// Always blocked, wherever the address is smuggled in (IPv4-mapped,
// IPv4-compatible, NAT64, 6to4): link-local, cloud metadata, unspecified,
// multicast, broadcast and reserved space. Loopback and private LAN stay
// reachable (agents preview dev servers), except that ownPorts on loopback
// or on one of the host's own addresses (local) are blocked, so the browser
// can't reach yoakeland itself.
func Blocked(ip netip.Addr, port uint16, ownPorts []uint16, local []netip.Addr) bool {
	ip = canonical(ip)
	if alwaysBlocked(ip) {
		return true
	}
	return slices.Contains(ownPorts, port) && (ip.IsLoopback() || slices.Contains(local, ip))
}

func canonical(ip netip.Addr) netip.Addr {
	ip = ip.WithZone("")
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	return ip
}

func alwaysBlocked(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() ||
		slices.Contains(blockedAddrs, ip) {
		return true
	}
	for _, p := range blockedNets {
		if p.Contains(ip) {
			return true
		}
	}
	// A translated address is never legitimately loopback either.
	if v4, ok := embeddedV4(ip); ok {
		return alwaysBlocked(v4) || v4.IsLoopback()
	}
	return false
}

var (
	nat64   = netip.MustParsePrefix("64:ff9b::/96")
	compat4 = netip.MustParsePrefix("::/96")
	sixTo4  = netip.MustParsePrefix("2002::/16")
)

func embeddedV4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case nat64.Contains(ip), compat4.Contains(ip) && !ip.IsLoopback():
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixTo4.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// localAddrs lists this host's interface addresses.
func localAddrs() []netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	var out []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, canonical(ip))
			}
		}
	}
	return out
}
