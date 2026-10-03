package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// maxGrantDays bounds a manual entitlement. A grant is a stopgap ("try it for two
// weeks", "cover them while the payment sorts itself out"), not a licence: an
// unbounded one would quietly become permanent free access nobody remembers
// giving. Two years is far past any plausible stopgap while still letting an
// operator hand out a long courtesy period.
const maxGrantDays = 730

// EntitlementHandler serves the "what may this tenant use" surface: the tenant's
// own read, and the admin grant/revoke. Quantities (how many nodes, how much
// traffic) stay with the billing/limits handlers - this is only about kind.
type EntitlementHandler struct {
	state *AppState
}

func NewEntitlementHandler(state *AppState) *EntitlementHandler {
	return &EntitlementHandler{state: state}
}

// entitlementResponse is the wire shape for both reads.
type entitlementResponse struct {
	Success bool `json:"success"`
	services.Entitlement
}

// GetMine GET /api/me/entitlement - the caller's own entitlement.
//
// Drives the tenant UI: whether to offer "add your own node", whether to offer a
// route, and what to say when neither is available. Authed-exempt (own data).
func (h *EntitlementHandler) GetMine(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	ent, err := services.EffectiveEntitlement(h.state.Store, userID, time.Now(), h.state.StoreEnabled, IsAdmin(r))
	if err != nil {
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entitlementResponse{Success: true, Entitlement: ent})
}

// GetForUser GET /api/admin/users/{id}/entitlement - RequireCap("plans.read").
func (h *EntitlementHandler) GetForUser(w http.ResponseWriter, r *http.Request) {
	userID := mux.Vars(r)["id"]
	ent, err := services.EffectiveEntitlement(h.state.Store, userID, time.Now(), h.state.StoreEnabled, h.subjectIsAdmin(userID))
	if err != nil {
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entitlementResponse{Success: true, Entitlement: ent})
}

// grantRequest is the admin grant body. Days is a duration from NOW rather than
// an absolute date: "give them 14 days" is what an operator actually means, and
// it removes every timezone question from the UI.
type grantRequest struct {
	Kind string `json:"kind"`
	Days int    `json:"days"`
}

// grantKinds expands the request's kind into the kinds actually written. Only
// "both" expands; it exists so the fan-out is one named thing rather than an if
// in the middle of the handler.
func grantKinds(kind string) []string {
	if kind == services.EntitlementBoth {
		return []string{services.EntitlementByon, services.EntitlementRouteOnly}
	}
	return []string{kind}
}

// Grant POST /api/admin/users/{id}/entitlement - RequireCap("plans.write").
//
// Grants BYON and/or route-only for N days. Additive to whatever the tenant's
// plan already allows, so a later store subscription extends the answer instead
// of colliding with the grant.
func (h *EntitlementHandler) Grant(w http.ResponseWriter, r *http.Request) {
	userID := mux.Vars(r)["id"]
	if !guardAccountTerms(w, r, h.state, userID) {
		return
	}
	var req grantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	kind := strings.TrimSpace(strings.ToLower(req.Kind))
	switch kind {
	case services.EntitlementByon, services.EntitlementRouteOnly, services.EntitlementBoth:
	default:
		sendJSONError(w, `kind must be "byon", "route_only" or "both"`, http.StatusBadRequest)
		return
	}
	if req.Days < 1 || req.Days > maxGrantDays {
		sendJSONError(w, "days must be between 1 and 730", http.StatusBadRequest)
		return
	}

	// Guard against granting to a user who does not exist: the upsert would
	// otherwise fail on the foreign key with an opaque 500.
	if u, err := h.state.Store.GetUserByID(userID); err != nil || u == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	expires := time.Now().Add(time.Duration(req.Days) * 24 * time.Hour)
	actor, _ := r.Context().Value("userID").(string)
	// Per KIND, so granting one does not end the other. "both" is still accepted
	// and now means what it says - two grants with the same deadline - rather
	// than one grant that happens to cover two things.
	for _, k := range grantKinds(kind) {
		if err := h.state.Store.SetUserManualEntitlementKind(userID, k, &expires, actor); err != nil {
			sendJSONError(w, "Save failed", http.StatusInternalServerError)
			return
		}
	}
	// No quantity is written here, and that is the point.
	//
	// A grant used to write the count into max_nodes / max_links - the SAME
	// columns the store pushes a purchase into, and the same ones the resolver
	// reads as evidence that a subscription exists. So granting made a tenant
	// indistinguishable from a paying one, and the override outlived the grant:
	// when the deadline passed they kept the product forever.
	//
	// A grant is worth exactly one machine of its kind, and that is derived
	// where the ceiling is read (services.EffectiveLimits) rather than stored.
	// Nothing writes a purchase column except a purchase.
	LogIdentityAudit(h.state, r, AuditEventEntitlementGranted, actor, userID, map[string]interface{}{
		"kind":       kind,
		"days":       req.Days,
		"expires_at": expires,
	})

	ent, _ := services.EffectiveEntitlement(h.state.Store, userID, time.Now(), h.state.StoreEnabled, h.subjectIsAdmin(userID))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entitlementResponse{Success: true, Entitlement: ent})
}

// subjectIsAdmin answers the entitlement question about the user NAMED IN THE
// PATH, not about the administrator asking. The caller's own admin flag says
// nothing about them, and using it here would make every customer's entitlement
// screen read "unlimited" whenever an admin opened it.
//
// A lookup failure answers false: reporting a customer as unlimited because a
// query failed is the worse of the two wrong answers.
func (h *EntitlementHandler) subjectIsAdmin(userID string) bool {
	u, err := h.state.Store.GetUserByID(userID)
	return err == nil && u != nil && u.IsAdmin
}

// Revoke DELETE /api/admin/users/{id}/entitlement - RequireCap("plans.write").
//
// Clears the grant. Whatever the tenant's plan allows is untouched, so this is
// "take back what I gave", not "cut them off" (that is the billing status).
func (h *EntitlementHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	userID := mux.Vars(r)["id"]
	if !guardAccountTerms(w, r, h.state, userID) {
		return
	}
	// ?kind= takes back ONE of the two. Without it the whole grant goes, which
	// is what every existing caller means and what the button used to do.
	switch kind := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("kind"))); kind {
	case "":
		if err := h.state.Store.SetUserManualEntitlement(userID, "", nil, ""); err != nil {
			sendJSONError(w, "Save failed", http.StatusInternalServerError)
			return
		}
	case services.EntitlementByon, services.EntitlementRouteOnly:
		if err := h.state.Store.SetUserManualEntitlementKind(userID, kind, nil, ""); err != nil {
			sendJSONError(w, "Save failed", http.StatusInternalServerError)
			return
		}
	default:
		sendJSONError(w, `kind must be "byon" or "route_only", or omitted to revoke both`, http.StatusBadRequest)
		return
	}
	actor, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEventEntitlementRevoked, actor, userID, nil)

	ent, _ := services.EffectiveEntitlement(h.state.Store, userID, time.Now(), h.state.StoreEnabled, h.subjectIsAdmin(userID))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entitlementResponse{Success: true, Entitlement: ent})
}
