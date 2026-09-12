package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"dylaris-core/services"
)

// The hole this closes: services.EffectiveEntitlement existed, GET
// /api/entitlement reported it, the panel rendered it - and not one write path
// consulted it. A freshly registered account that had never touched the store
// could mint node enroll tokens, warp keys and link kits, because the only guard
// on those paths was a numeric cap that skips itself when the number is zero:
//
//	if lim.MaxNodes > 0 { ...check... }
//
// A tenant who bought nothing has no override row, so MaxNodes is 0, so the
// check does not run, so they are uncapped. The one value that means "bought
// nothing" was the one value that disabled the limit.
//
// The entitlement is now the gate and the cap is the ceiling behind it. Both are
// needed: the entitlement answers "may they at all", the cap answers "how many".

// entitlementDenial is why a request was refused, in the words the tenant needs.
// Split by cause on purpose - "link your account" and "buy a plan" are different
// actions, and telling someone to buy a plan they already bought because the
// join is missing is the worst of the three outcomes.
type entitlementDenial struct {
	Code    string
	Message string
	Status  int
}

const (
	// DenyNotLinked: the store is live, this account is not joined to a store
	// account, so nothing they may have bought can reach Core.
	DenyNotLinked = "store_not_linked"
	// DenyNoEntitlement: joined, but holding no subscription that grants this.
	DenyNoEntitlement = "no_entitlement"
	// DenySuspended: held something, currently stopped.
	DenySuspended = "billing_suspended"
)

// requireEntitlement resolves what the tenant may use and refuses with a reason
// the panel can act on. kind is EntitlementByon or EntitlementRouteOnly.
//
// Takes the request rather than a bare context because the answer depends on WHO
// is asking: an administrator is not a customer, and EffectiveEntitlement reads
// it that way. Without it the owner of a hosted install could not mint an enroll
// token for their own machine.
//
// Returns true when the request may proceed. On false it has already written the
// response.
func (s *AppState) requireEntitlement(r *http.Request, w http.ResponseWriter, userID, kind string) bool {
	ctx := r.Context()
	// BYON off: there is no entitlement plane, and every one of these endpoints
	// is already unreachable. Nothing to decide.
	if s.FeatureFlags == nil || !s.FeatureFlags.IsBYONEnabled(ctx) {
		return true
	}

	ent, err := services.EffectiveEntitlement(s.Store, userID, time.Now(), s.StoreEnabled, IsAdmin(r))
	if err != nil {
		sendJSONError(w, "Could not determine your subscription", http.StatusInternalServerError)
		return false
	}

	allowed := ent.Byon
	if kind == services.EntitlementRouteOnly {
		allowed = ent.RouteOnly
	}
	if allowed {
		return true
	}

	d := s.denialFor(ctx, ent, userID)
	sendJSONErrorCode(w, d.Message, d.Code, d.Status)
	return false
}

// sendJSONErrorCode is sendJSONError plus a stable machine-readable code. The
// panel needs to tell the three denials apart to render the right call to
// action, and matching on the prose would break the first time it is reworded.
func sendJSONErrorCode(w http.ResponseWriter, message, code string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": message,
		"code":    code,
	})
}

// denialFor picks the message. Suspension is named first because it is the only
// one where the tenant DID buy the thing and an unhelpful "buy a plan" would be
// actively wrong.
func (s *AppState) denialFor(ctx context.Context, ent services.Entitlement, userID string) entitlementDenial {
	if ent.Source == services.EntitlementSourceSuspended {
		return entitlementDenial{
			Code:    DenySuspended,
			Message: "Your subscription is suspended. Settle the outstanding invoice in the store to continue.",
			Status:  http.StatusForbidden,
		}
	}
	if s.StoreEnabled && !s.storeLinkedBestEffort(ctx, userID) {
		return entitlementDenial{
			Code:    DenyNotLinked,
			Message: "Your panel account is not connected to the Dylaris store yet. Open your account menu, choose Dylaris store, and connect it - a subscription raises no limits here until you do.",
			Status:  http.StatusForbidden,
		}
	}
	return entitlementDenial{
		Code:    DenyNoEntitlement,
		Message: "This needs an active plan. Subscribe in the Dylaris store, then come back - your limits update within seconds.",
		Status:  http.StatusForbidden,
	}
}

// storeLinkedBestEffort answers "is this account joined to a store account", and
// only ever narrows the message. A storefront outage reports LINKED, so the
// tenant is told to buy a plan rather than to fix a join that may be fine: the
// generic message is right in both cases, while "you are not connected" would be
// a confident lie produced by our own network error.
func (s *AppState) storeLinkedBestEffort(ctx context.Context, userID string) bool {
	linked, _, err := s.probeStoreLink(ctx, userID)
	if err != nil {
		log.Printf("entitlement: link-status lookup failed for %s, assuming linked: %v", userID, err)
		return true
	}
	return linked
}
