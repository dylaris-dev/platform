package billing

import (
	"testing"

	"dylaris-core/services"
)

// The retired "global" scope must be refused on WRITE, not just ignored on read.
//
// A write that stored it would succeed, show the value back on the settings
// screen, and limit nobody - the resolver no longer asks that scope. An operator
// would have set a cap and been shown it working.
func TestValidTrafficScope(t *testing.T) {
	tests := []struct {
		scope string
		want  bool
	}{
		{"user_default", true},
		{"user:9f1c2d3e", true},
		{"global", false},
		{"", false},
		{"user:", false}, // a scope for nobody: stores, never resolves
		{"nonsense", false},
	}
	for _, tt := range tests {
		if got := validTrafficScope(tt.scope); got != tt.want {
			t.Errorf("validTrafficScope(%q) = %v, want %v", tt.scope, got, tt.want)
		}
	}
}

// A write for a non-regional kind lands on the one row the resolver reads,
// whatever region the client sent.
//
// Storing it as typed would put the number under a region nothing ever asks
// about: a limit an operator set, saw echoed back, and that enforces nothing.
func TestNonRegionalWritesFoldOntoOneRow(t *testing.T) {
	for _, region := range []string{"eu-central", "us-east", "", "*"} {
		if got := services.TrafficLimitRegion(region, "relay"); got != services.TrafficRegionAny {
			t.Errorf("relay write for region %q stored under %q, want %q", region, got, services.TrafficRegionAny)
		}
	}
	// Player traffic keeps its region: that is the whole reason it is per region.
	if got := services.TrafficLimitRegion("ap-southeast", services.TrafficKindEdge); got != "ap-southeast" {
		t.Errorf("edge write stored under %q, want ap-southeast", got)
	}
}

// The '*' row has to survive the label check that every real region passes.
// Without the carve-out a relay limit could not be saved at all.
func TestRegionAnyIsNotRejectedAsALabel(t *testing.T) {
	if validTrafficLabel(services.TrafficRegionAny) {
		t.Fatalf("%q unexpectedly passes validTrafficLabel; the handler's carve-out would be dead code",
			services.TrafficRegionAny)
	}
}
