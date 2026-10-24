package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/services"
)

// GetDeployConfig GET /api/warp/deploy-config - a tenant who can actually
// deploy something, or an admin.
//
// Deliberately not admin-only: the tenant who mints a BYON key on /nodes is the
// one who needs this, and withholding it would only mean handing it over by
// email instead. It is an RFC1918 CIDR for an overlay nobody reaches without
// their own authenticated warp key, and the hash of a certificate every client
// of the gRPC port is shown in its handshake, so it authorizes nothing on its
// own.
//
// It is not open to EVERY authenticated account either, which is where it
// started. Self-registration is on, so that turned "guess which range the
// overlay uses" into "read it" for anyone who signs up. The people the argument
// above is about are exactly the ones this still answers.
func (h *WarpHandler) GetDeployConfig(w http.ResponseWriter, r *http.Request) {
	if !h.mayReadDeployConfig(r) {
		handlers.SendJSONError(w, "This is only available with a BYON or route-only subscription", http.StatusForbidden)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  h.state.DeployConfig(),
	})
}

// mayReadDeployConfig reports whether the caller has anything to deploy.
//
// It mirrors requireEntitlement rather than calling it, because that one writes
// its own refusal and this question is "either product", not one of them. The
// both-products-off case follows it exactly: with no entitlement plane there is
// nothing to decide, and on a self-hosted platform with BYON and route-only
// switched off every reader is the operator anyway.
//
// A failed lookup is a refusal here. The panel already falls back to its
// placeholders when this call does not answer, so failing closed costs a
// snippet, while failing open would be the thing the gate exists to prevent.
func (h *WarpHandler) mayReadDeployConfig(r *http.Request) bool {
	if handlers.IsAdmin(r) {
		return true
	}
	if h.state.FeatureFlags == nil {
		return true
	}
	ctx := r.Context()
	byonOn := h.state.FeatureFlags.IsBYONEnabled(ctx)
	routeOnlyOn := h.state.FeatureFlags.IsRouteOnlyEnabled(ctx)
	if !byonOn && !routeOnlyOn {
		return true
	}
	userID, _ := ctx.Value("userID").(string)
	ent, err := services.EffectiveEntitlement(h.state.Store, userID, time.Now(), h.state.StoreEnabled, false)
	if err != nil {
		log.Printf("deploy-config: could not resolve entitlement for %s: %v", userID, err)
		return false
	}
	return (byonOn && ent.Byon) || (routeOnlyOn && ent.RouteOnly)
}
