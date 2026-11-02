package handlers

import (
	"testing"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/services"
)

// A tenant's link kit is fenced from admins whenever route-only is on, not only
// with BYON: route-only on beside BYON off would otherwise let any admin roll a
// customer's kit and read its new key.
func TestAKitIsItsOwnersWhileRouteOnlyIsOn(t *testing.T) {
	cases := []struct {
		name     string
		settings *handlertest.SettingsStore
		want     bool
	}{
		{"route-only on, BYON off", &handlertest.SettingsStore{KV: map[string]string{"feature_route_only_enabled": "true"}}, true},
		{"BYON on", &handlertest.SettingsStore{KV: map[string]string{"feature_byon_enabled": "true"}}, true},
		{"neither", &handlertest.SettingsStore{KV: map[string]string{}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &AppState{FeatureFlags: services.NewFeatureFlags(c.settings)}
			if got := access.KitOwnedByOther(s.FeatureFlags, "tenant", "admin"); got != c.want {
				t.Errorf("kitOwnedByOther = %v, want %v", got, c.want)
			}
		})
	}
	if access.KitOwnedByOther(nil, "tenant", "tenant") {
		t.Error("the owner is not a stranger to their own kit")
	}
}
