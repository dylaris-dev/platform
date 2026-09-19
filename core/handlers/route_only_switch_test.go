package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
)

type flagSettings map[string]string

func (f flagSettings) GetSetting(key string) (string, error) { return f[key], nil }

// Route-only switched off closes claiming addresses too, not only minting kits.
// The entitlement gate answers "nothing to decide" while the product is off, so
// without its own gate the handler would take addresses without checking the
// subscription at all.
func TestCreateLinkRouteIsClosedWithRouteOnlyOff(t *testing.T) {
	state := &AppState{FeatureFlags: services.NewFeatureFlags(flagSettings{
		"feature_byon_enabled":       "true",
		"feature_route_only_enabled": "false",
	})}
	h := &GatewayHandler{state: state}
	req := httptest.NewRequest(http.MethodPost, "/api/gateway/link-routes",
		strings.NewReader(`{"linkId":"link-x","subdomain":"a","targetHost":"127.0.0.1"}`))
	//nolint:staticcheck // the handlers read a plain string key; matching them is the point
	req = req.WithContext(context.WithValue(req.Context(), "userID", "u1"))
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

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
