package handlers

import (
	"testing"

	"dylaris-core/services"
)

type flagSettings map[string]string

func (f flagSettings) GetSetting(key string) (string, error) { return f[key], nil }

// A tenant's link kit is fenced from admins whenever route-only is on, not only
// with BYON: route-only on beside BYON off would otherwise let any admin roll a
// customer's kit and read its new key.
func TestAKitIsItsOwnersWhileRouteOnlyIsOn(t *testing.T) {
	cases := []struct {
		name     string
		settings flagSettings
		want     bool
	}{
		{"route-only on, BYON off", flagSettings{"feature_route_only_enabled": "true"}, true},
		{"BYON on", flagSettings{"feature_byon_enabled": "true"}, true},
		{"neither", flagSettings{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &AppState{FeatureFlags: services.NewFeatureFlags(c.settings)}
			if got := s.kitOwnedByOther("tenant", "admin"); got != c.want {
				t.Errorf("kitOwnedByOther = %v, want %v", got, c.want)
			}
		})
	}
	if (&AppState{}).kitOwnedByOther("tenant", "tenant") {
		t.Error("the owner is not a stranger to their own kit")
	}
}
