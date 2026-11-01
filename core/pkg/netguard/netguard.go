// Package netguard is the one rule for which addresses Core may dial on behalf
// of a tenant. It is a leaf package so that both the import fetcher (services)
// and the backup storage client (storage/backup) can use it: services imports
// storage/backup, so the rule could not live in services and be shared.
package netguard

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
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
	Control:   control(func() []netip.Prefix { return nil }),
}

// TenantDialer is Dialer for a tenant's OWN storage endpoint (backups,
// templates). It additionally lets through the private networks the operator
// listed in STORAGE_PRIVATE_ALLOW (SetPrivateAllow), so a self-hosted install
// can point users at a MinIO on the LAN. It is a separate dialer on purpose:
// the import fetcher and every other netguard user must never be widened by
// that setting.
var TenantDialer = &net.Dialer{
	Timeout:   10 * time.Second,
	KeepAlive: -1,
	Control:   control(func() []netip.Prefix { return *privateAllow.Load() }),
}

func control(allow func() []netip.Prefix) func(string, string, syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !Allowed(ip, allow()) {
			return fmt.Errorf("blocked non-public address: %s", address)
		}
		return nil
	}
}

var privateAllow atomic.Pointer[[]netip.Prefix]

func init() { privateAllow.Store(&[]netip.Prefix{}) }

// SetPrivateAllow installs the operator's STORAGE_PRIVATE_ALLOW list for
// TenantDialer. Called once at startup; nil or empty is today's behaviour.
func SetPrivateAllow(allow []netip.Prefix) {
	cp := append([]netip.Prefix(nil), allow...)
	privateAllow.Store(&cp)
}

// PrivateAllow returns the installed list as CIDR strings, for the node
// commands that must apply the same rule to their presigned transfers. Nil
// when nothing is listed, so callers can omit the field.
func PrivateAllow() []string {
	list := *privateAllow.Load()
	if len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	for i, p := range list {
		out[i] = p.String()
	}
	return out
}

// Allowed reports whether ip may be dialed for a tenant given an operator's
// allow list: a public address always, a private one only inside a listed
// prefix. Loopback, link-local (the cloud metadata service lives there),
// unspecified and multicast are refused even when listed, so an over-broad
// entry such as 0.0.0.0/0 cannot reopen Core's own sockets or the metadata
// endpoint.
func Allowed(ip net.IP, allow []netip.Prefix) bool {
	if !Disallowed(ip) {
		return true
	}
	if len(allow) == 0 || neverAllowed(ip) {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range allow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func neverAllowed(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// ParseAllowList parses a comma-separated CIDR list (STORAGE_PRIVATE_ALLOW).
// Blank entries are skipped; any invalid entry is an error naming it, because
// a typo silently dropped would leave the operator debugging a refusal.
func ParseAllowList(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
