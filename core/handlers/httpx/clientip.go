package httpx

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// trustedProxies holds the reverse-proxy networks whose X-Forwarded-For header
// Core believes. Stored behind an atomic pointer so SetTrustedProxies can be
// called once at startup and ClientIP can read it lock-free on every request.
// A nil value means no proxy is trusted and XFF is ignored entirely.
var trustedProxies atomic.Pointer[[]*net.IPNet]

// SetTrustedProxies installs the trusted reverse-proxy networks, from
// config.TrustedProxyCIDRs. Called once from main before serving.
func SetTrustedProxies(cidrs []*net.IPNet) {
	trustedProxies.Store(&cidrs)
}

// ClientIP returns the originating client IP for rate limiting and the audit
// log. Returns "" only when RemoteAddr is unparseable, so the downstream
// NULLIF(...,”)::inet cast keeps the DB row clean.
//
// X-Forwarded-For is attacker-controlled and must be treated as such. A client
// can put anything in it; the only entries that can be believed are the ones a
// trusted proxy appended. So the peer that actually connected (RemoteAddr) is
// the anchor:
//
//   - If that peer is not a trusted proxy, it IS the client (a direct
//     connection, or an untrusted one). XFF is ignored entirely, which is what
//     stops a direct client from forging it.
//   - If the peer is a trusted proxy, walk XFF from the RIGHT - the end the
//     proxy appends to - skipping further trusted hops, and take the first
//     untrusted address. That is the real client. A forged entry a client
//     inserted sits to the LEFT of what the proxy appended, so the walk never
//     reaches it.
//
// With no trusted proxies configured the peer is never trusted, so this always
// returns RemoteAddr - correct for a directly exposed Core, and safe (never
// spoofable) everywhere else.
func ClientIP(r *http.Request) string {
	peer := remoteAddrIP(r.RemoteAddr)

	if !isTrustedProxy(peer) {
		return peer
	}

	// The peer is a trusted proxy; the address it appended to XFF is the one to
	// believe. Walk right-to-left past any further trusted hops.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			entry := strings.TrimSpace(parts[i])
			if entry == "" {
				continue
			}
			ip := net.ParseIP(entry)
			if ip == nil {
				// An unparseable entry could be a client-inserted junk value;
				// stop rather than skipping past it into forgeable territory.
				break
			}
			if isTrustedProxyIP(ip) {
				continue
			}
			return entry
		}
	}

	// XFF was absent, empty, or every entry was a trusted hop. A proxy that
	// sets X-Real-IP instead is believed here because the peer is trusted; a
	// direct client's forged X-Real-IP never reaches this point (peer untrusted
	// returns above).
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	return peer
}

// remoteAddrIP strips the port from a host:port RemoteAddr. Returns the input
// unchanged when it has no port (already a bare IP).
func remoteAddrIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

func isTrustedProxy(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	return isTrustedProxyIP(ip)
}

func isTrustedProxyIP(ip net.IP) bool {
	nets := trustedProxies.Load()
	if nets == nil {
		return false
	}
	for _, n := range *nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
