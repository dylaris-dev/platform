package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/store"
)

func callDeployConfig(state *AppState, userID string, isAdmin bool) *httptest.ResponseRecorder {
	h := NewWarpHandler(state, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/warp/deploy-config", nil)
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	rec := httptest.NewRecorder()
	h.GetDeployConfig(rec, r.WithContext(ctx))
	return rec
}

// The deploy snippet names the overlay's CIDR and the gRPC certificate
// fingerprint. Neither authorizes anything on its own - that argument is in the
// handler and it stands - but the endpoint carried NO check at all, and
// self-registration is on, so anyone who signed up could read which range the
// overlay uses instead of having to guess it.
//
// The people the openness argument is about are tenants who mint a BYON or
// route-only key. They still get it; an account that bought neither does not.
func TestDeployConfigIsForTenantsWhoCanDeploySomething(t *testing.T) {
	t.Run("an account that bought nothing", func(t *testing.T) {
		rec := callDeployConfig(newEntGateState(nil, true), "u-nothing", false)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a BYON tenant", func(t *testing.T) {
		state := newEntGateState(&store.UserBilling{Status: "active", MaxNodes: ptrI64(1)}, true)
		rec := callDeployConfig(state, "u-byon", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Success bool `json:"success"`
			Config  any  `json:"config"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if !body.Success || body.Config == nil {
			t.Errorf("the tenant who needs the snippet did not get it: %s", rec.Body.String())
		}
	})

	t.Run("an admin", func(t *testing.T) {
		rec := callDeployConfig(newEntGateState(nil, true), "u-admin", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	// The both-products-off case, which mirrors requireEntitlement: with no
	// entitlement plane there is nothing to decide, and on a self-host with BYON
	// and route-only switched off every reader is the operator.
	t.Run("a platform with the products switched off", func(t *testing.T) {
		state := newEntGateState(nil, true)
		state.Store.(*entGateStore).settings = map[string]string{}
		rec := callDeployConfig(state, "u-selfhost", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}
