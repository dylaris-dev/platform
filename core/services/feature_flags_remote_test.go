package services

import (
	"context"
	"testing"
)

// With the managed gateway as the provider, the getters that OPEN BYON and
// route-only answer false whatever is stored; a local or unknown mode keeps the
// stored answer. The ownership fences ignore the mode entirely: a tenant's
// machine stays theirs across a switch to remote.
func TestRemoteModeTurnsTenancyFlagsOff(t *testing.T) {
	ctx := context.Background()
	on := stubSettings{m: map[string]string{"feature_byon_enabled": "true", "feature_route_only_enabled": "true"}}
	mode := func(remote, known bool) func() (bool, bool) { return func() (bool, bool) { return remote, known } }
	tests := []struct {
		name       string
		mode       func() (bool, bool)
		wantOpen   bool
		wantFences bool
	}{
		{"no mode wired", nil, true, true},
		{"local", mode(false, true), true, true},
		{"remote", mode(true, true), false, true},
		{"unknown keeps the stored flags", mode(true, false), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFeatureFlags(on)
			f.SetGatewayRemote(tt.mode)
			for name, v := range map[string]bool{
				"IsBYONEnabled":      f.IsBYONEnabled(ctx),
				"IsRouteOnlyEnabled": f.IsRouteOnlyEnabled(ctx),
				"IsTenancyEnabled":   f.IsTenancyEnabled(ctx),
			} {
				if v != tt.wantOpen {
					t.Errorf("%s = %v, want %v", name, v, tt.wantOpen)
				}
			}
			for name, v := range map[string]bool{
				"BYONOwnershipInForce":      f.BYONOwnershipInForce(ctx),
				"RouteOnlyOwnershipInForce": f.RouteOnlyOwnershipInForce(ctx),
			} {
				if v != tt.wantFences {
					t.Errorf("%s = %v, want %v (the fences keep the stored answer in every mode)", name, v, tt.wantFences)
				}
			}
		})
	}
}
