// Package netguard is the one rule for which addresses Core may dial on behalf
// of a tenant. It is a leaf package so that both the import fetcher (services)
// and the backup storage client (storage/backup) can use it: services imports
// storage/backup, so the rule could not live in services and be shared.
package netguard

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

// Disallowed reports whether an IP must never be dialed for a tenant. It blocks
// loopback, private (RFC1918 / IPv6 ULA), CGNAT, link-local (including the
// cloud metadata endpoint 169.254.169.254 and fe80::/10), unspecified, and
// multicast ranges. IPv4-mapped IPv6 is normalized first so that e.g.
// ::ffff:10.0.0.1 is caught as private.
func Disallowed(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// 100.64.0.0/10 (CGNAT, RFC 6598) is a standard SSRF blocklist range that
	// IsPrivate does not cover.
	if len(ip) == net.IPv4len && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// Dialer validates the concrete post-resolution IP right before connect.
// Because Control runs on the already-resolved address (not the hostname), it
// also defeats DNS rebinding: a name that resolves to a public IP at lookup
// time but a private IP at dial time is still rejected here.
var Dialer = &net.Dialer{
	Timeout:   10 * time.Second,
	KeepAlive: -1,
	Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || Disallowed(ip) {
			return fmt.Errorf("blocked non-public address: %s", address)
		}
		return nil
	},
}
