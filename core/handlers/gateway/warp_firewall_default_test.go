package gateway

import (
	"strings"
	"testing"

	"dylaris-core/handlers"
)

// Core and the gateway's warp leader each compile their own copy of this list,
// and the leader programs ITS copy before it has ever spoken to Core. So the
// two defaults have to agree, and the one thing they must agree on is that the
// edge tunnel port is not in them.
//
// A customer machine has to reach an edge over the internet. Through the
// overlay its players share one warp leader's bandwidth with that same
// customer's Redis traffic and Beam uploads, and drop every session whenever
// the leader restarts. This allowlist is the only place that can enforce it:
// LINK_EXTERNAL, NODE_EXTERNAL and NODE_TAGS are all env on the customer's own
// host, so nothing the machine declares about itself is evidence.
func TestDefaultWarpSpokeAllowlistExcludesTheEdgeTunnel(t *testing.T) {
	for _, p := range strings.Split(handlers.DefaultWarpSpokeAllowedPorts, ",") {
		if strings.TrimSpace(p) == "25560" {
			t.Fatalf("25560 is in %q: a spoke can tunnel player traffic through the overlay",
				handlers.DefaultWarpSpokeAllowedPorts)
		}
	}
	// The three that must stay. Without them a customer machine cannot reach
	// Redis, Core's gRPC or the beam relay - which is every customer broken, and
	// an empty list is not hardening.
	for _, want := range []string{"6379", "25501", "25551"} {
		found := false
		for _, p := range strings.Split(handlers.DefaultWarpSpokeAllowedPorts, ",") {
			if strings.TrimSpace(p) == want {
				found = true
			}
		}
		if !found {
			t.Errorf("port %s missing from %q", want, handlers.DefaultWarpSpokeAllowedPorts)
		}
	}
}
