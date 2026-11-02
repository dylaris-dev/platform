package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/config"
)

// withTrustedProxies installs a proxy set for one test and restores whatever
// was there. The package var is process-global, so tests that touch it must not
// leak into each other.
func withTrustedProxies(t *testing.T, raw string) {
	t.Helper()
	prev := trustedProxies.Load()
	t.Cleanup(func() { trustedProxies.Store(prev) })
	SetTrustedProxies(config.ParseTrustedProxyCIDRs(raw))
}

func reqWith(remoteAddr, xff, xrealip string) *http.Request {
	r := &http.Request{RemoteAddr: remoteAddr, Header: http.Header{}}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if xrealip != "" {
		r.Header.Set("X-Real-IP", xrealip)
	}
	return r
}

// TestClientIP_DefeatsForgedXFF is the whole point of the change. Behind a
// trusted proxy the real client sits at the RIGHT of X-Forwarded-For, where the
// proxy appended it; anything the client forged is to the LEFT and must never
// be believed, or every request could claim a different IP and slip the rate
// limiter.
func TestClientIP_DefeatsForgedXFF(t *testing.T) {
	withTrustedProxies(t, "") // private-range default; the proxy is on 10.x

	// Attacker (real IP 203.0.113.7) forges a leading entry; the trusted proxy
	// on 10.0.0.5 appends the real peer.
	r := reqWith("10.0.0.5:4000", "1.2.3.4, 203.0.113.7", "")

	got := ClientIP(r)
	if got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want the proxy-appended real client 203.0.113.7, NOT the forged 1.2.3.4", got)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		trusted    string
		remoteAddr string
		xff        string
		xRealIP    string
		want       string
	}{
		{
			name:       "direct client, no proxy configured",
			trusted:    "none",
			remoteAddr: "203.0.113.7:5000",
			xff:        "1.2.3.4", // forged, must be ignored
			want:       "203.0.113.7",
		},
		{
			name:       "direct client with a proxy set but peer not trusted",
			trusted:    "",
			remoteAddr: "203.0.113.7:5000",
			xff:        "1.2.3.4", // peer is public, so XFF is ignored
			want:       "203.0.113.7",
		},
		{
			name:       "single trusted proxy",
			trusted:    "",
			remoteAddr: "10.0.0.5:4000",
			xff:        "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "chain of two trusted proxies",
			trusted:    "",
			remoteAddr: "10.0.0.5:4000",
			xff:        "203.0.113.7, 172.16.0.9",
			want:       "203.0.113.7",
		},
		{
			name:       "forged entry to the left of the real client",
			trusted:    "",
			remoteAddr: "10.0.0.5:4000",
			xff:        "9.9.9.9, 203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "X-Real-IP is believed from a trusted peer when XFF is absent",
			trusted:    "",
			remoteAddr: "10.0.0.5:4000",
			xRealIP:    "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "X-Real-IP is ignored from a direct client",
			trusted:    "none",
			remoteAddr: "203.0.113.7:5000",
			xRealIP:    "1.2.3.4", // forged
			want:       "203.0.113.7",
		},
		{
			name:       "unparseable XFF entry stops the walk at the trusted peer",
			trusted:    "",
			remoteAddr: "10.0.0.5:4000",
			xff:        "garbage",
			want:       "10.0.0.5",
		},
		{
			name:       "IPv6 peer and client",
			trusted:    "",
			remoteAddr: "[fc00::5]:4000",
			xff:        "2001:db8::1",
			want:       "2001:db8::1",
		},
		{
			name:       "bare RemoteAddr without a port",
			trusted:    "none",
			remoteAddr: "203.0.113.7",
			want:       "203.0.113.7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTrustedProxies(t, tt.trusted)
			got := ClientIP(reqWith(tt.remoteAddr, tt.xff, tt.xRealIP))
			if got != tt.want {
				t.Fatalf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClientIP_NoTrustSetBehavesLikeDirect pins the zero-value: before
// SetTrustedProxies is ever called (or after "none"), no peer is trusted, so
// RemoteAddr is always returned and no header can override it.
func TestClientIP_NoTrustSetBehavesLikeDirect(t *testing.T) {
	prev := trustedProxies.Load()
	t.Cleanup(func() { trustedProxies.Store(prev) })
	var empty []*net.IPNet
	trustedProxies.Store(&empty)

	got := ClientIP(reqWith("10.0.0.5:4000", "1.2.3.4", "9.9.9.9"))
	if got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want the peer 10.0.0.5 when nothing is trusted", got)
	}
}

// TestTheBootSetterFeedsClientIPAndTheLimiter pins that the list main.go
// installs through SetTrustedProxies is the one ClientIP and the rate limiter
// read. A second copy of the list (left behind when this code moved out of
// handlers) would leave ClientIP with nothing trusted: every user behind the
// proxy would share one rate-limit bucket and one audit IP.
func TestTheBootSetterFeedsClientIPAndTheLimiter(t *testing.T) {
	prev := trustedProxies.Load()
	t.Cleanup(func() { trustedProxies.Store(prev) })
	SetTrustedProxies(config.ParseTrustedProxyCIDRs("10.0.0.0/8"))

	if got := ClientIP(reqWith("10.0.0.5:4000", "203.0.113.7", "")); got != "203.0.113.7" {
		t.Fatalf("behind the trusted proxy ClientIP = %q, want the forwarded client", got)
	}
	if got := ClientIP(reqWith("198.51.100.9:4000", "203.0.113.7", "")); got != "198.51.100.9" {
		t.Fatalf("from an untrusted peer ClientIP = %q, want the peer: XFF must be ignored", got)
	}

	// Two clients behind the same proxy keep separate budgets.
	l := NewIPRateLimiter()
	h := l.Limit(1, func(w http.ResponseWriter, r *http.Request) {})
	for _, client := range []string{"203.0.113.7", "203.0.113.8"} {
		rec := httptest.NewRecorder()
		h(rec, reqWith("10.0.0.5:4000", client, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("first request of %s got %d: the clients share one bucket", client, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h(rec, reqWith("10.0.0.5:4000", "203.0.113.7", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request of one client got %d, want 429", rec.Code)
	}
}
