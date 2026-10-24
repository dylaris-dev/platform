package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/services"
)

// Route-only switched off closes claiming addresses too, not only minting kits.
// The entitlement gate answers "nothing to decide" while the product is off, so
// without its own gate the handler would take addresses without checking the
// subscription at all.
func TestCreateLinkRouteIsClosedWithRouteOnlyOff(t *testing.T) {
	state := &handlers.AppState{FeatureFlags: services.NewFeatureFlags(flagSettings{
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
