package beam

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// allowLoopbackBeamDial relaxes the SSRF guard on the beam upstream fetches for
// the duration of one test, restoring it afterwards. Needed because
// httptest.NewServer binds 127.0.0.1, which services.SafeDialContext refuses -
// correctly, which is the whole point of the guard.
//
// Every test that stands up a fake manifest or binary upstream must call this;
// a test that does NOT is asserting the guard is on, which is what
// TestBeamUpstreamFetchesRefuseNonPublicAddresses below does.
func allowLoopbackBeamDial(t *testing.T) {
	t.Helper()
	prev := beamDial
	beamDial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	t.Cleanup(func() { beamDial = prev })
}

// The two beam upstream URLs (beam.download_link and beam.release_manifest) are
// operator-set strings Core fetches server-side, and the binary one is streamed
// straight back to a caller of the deliberately unauthenticated
// /api/beam/download. Without an SSRF guard on the dial, a settings.write
// holder - a delegatable panel capability, not admin - could point Core at an
// internal address and read the answer back out of it.
func TestBeamUpstreamFetchesRefuseNonPublicAddresses(t *testing.T) {
	cases := []struct {
		name string
		addr string
	}{
		{"loopback", "127.0.0.1:80"},
		{"private rfc1918", "10.0.0.5:80"},
		{"cloud metadata", "169.254.169.254:80"},
		{"ipv6 loopback", "[::1]:80"},
		{"ipv4-mapped private", "[::ffff:192.168.1.1]:80"},
		{"cgnat", "100.64.0.1:80"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := beamDialContext(ctx, "tcp", c.addr)
			if err == nil {
				conn.Close()
				t.Fatalf("dial to %s was allowed", c.addr)
			}
			// Assert it was the GUARD that refused, not the network. An
			// ordinary "connection refused" satisfies err != nil just as well,
			// so a bare non-nil check passes with the guard removed - which is
			// exactly what a perturbation run showed.
			if !strings.Contains(err.Error(), "blocked non-public address") {
				t.Fatalf("dial to %s failed for the wrong reason: %v", c.addr, err)
			}
		})
	}
}

// The guard must not be swallowing every dial: a public address still resolves
// past the check (the connection itself is not attempted here beyond the
// Control hook, so a refusal from the far end is fine - what must NOT appear is
// the guard's own "blocked non-public address" error).
func TestBeamDialAllowsAPublicAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := beamDialContext(ctx, "tcp", "203.0.113.1:80") // TEST-NET-3, never routed
	if conn != nil {
		conn.Close()
	}
	if err != nil && strings.Contains(err.Error(), "blocked non-public address") {
		t.Fatalf("a public address was refused by the guard: %v", err)
	}
}
