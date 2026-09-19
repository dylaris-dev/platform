package services

import (
	"context"
	"testing"
)

// Route-only had no switch of its own and rode on BYON's. Its own switch must
// not change anything for an operator who never sets it: unset, it follows BYON.
// Set, it answers for itself in either direction.
func TestRouteOnlyFollowsBYONUntilSet(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name          string
		settings      map[string]string
		wantRouteOnly bool
		wantTenancy   bool
	}{
		{"nothing set", map[string]string{}, false, false},
		{"BYON on, route-only unset", map[string]string{"feature_byon_enabled": "true"}, true, true},
		{"BYON on, route-only off", map[string]string{"feature_byon_enabled": "true", "feature_route_only_enabled": "false"}, false, true},
		{"route-only on without BYON", map[string]string{"feature_route_only_enabled": "true"}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ff := NewFeatureFlags(stubSettings{m: c.settings})
			if got := ff.IsRouteOnlyEnabled(ctx); got != c.wantRouteOnly {
				t.Errorf("IsRouteOnlyEnabled = %v, want %v", got, c.wantRouteOnly)
			}
			if got := ff.IsTenancyEnabled(ctx); got != c.wantTenancy {
				t.Errorf("IsTenancyEnabled = %v, want %v", got, c.wantTenancy)
			}
		})
	}
}
