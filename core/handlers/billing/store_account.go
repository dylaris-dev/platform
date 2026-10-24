package billing

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/services"
)

// The store account, read and changed from the panel.
//
// A tenant runs their servers here and pays over there, and until now the panel
// could only tell them WHETHER the two were joined. What they actually want to
// know - which subscription, how much of the traffic and backup allowance is
// gone, and whether going past it bills them or stops them - lived behind a
// store session the panel does not have.
//
// So Core asks on their behalf over the shared-key channel it already uses for
// link-status. Two rules make that safe, and both are load-bearing:
//
//   - The UUID is taken from the SESSION, never from the request body. A caller
//     who could name the account would be able to read anyone's invoice and move
//     anyone's consent.
//   - The store looks that UUID up in its own join table rather than trusting it
//     as an identity. Core proves it is Core with the shared key; the key says
//     nothing about which tenant.
//
// The numbers are not recomputed here. The store owns the money and already
// computes them for its own screens; a second implementation is the one that
// drifts, and the platform has been bitten by exactly that shape before - a
// pricing page promising an allowance nothing enforced.

// AccountSummary GET /api/store/account-summary - session-authed.
//
// Best-effort by design: a store outage returns the reachable=false shape with a
// reason, not an error page. The panel it is embedded in still works, and the
// tenant is told which of the two systems is quiet rather than being shown an
// account that appears to have nothing in it.
func (h *StoreHandler) AccountSummary(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "enabled": false})
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		handlers.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	summary, err := h.state.StoreAccountSummary(r.Context(), userID)
	if err != nil {
		log.Printf("store: account summary for %s: %v", userID, err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"enabled":   true,
			"reachable": false,
			"message":   "The store could not be reached, so your subscription details are not shown. Nothing about your servers is affected.",
		})
		return
	}
	summary["success"] = true
	summary["enabled"] = true
	summary["reachable"] = true
	summary["storeUrl"] = h.state.StoreURL
	// Whether an ADMIN GRANT is what entitles them, which the storefront cannot
	// know: a grant made in the panel writes Core's billing row and creates no
	// store subscription at all.
	//
	// Without it the panel told a granted tenant "There is no active
	// subscription to bill traffic against" - true of the store's database and
	// unreadable to somebody whose grant is working, because it describes their
	// entitlement as missing when it is simply not a purchase.
	if ent, eerr := services.EffectiveEntitlement(h.state.Store, userID, time.Now(), h.state.StoreEnabled, handlers.IsAdmin(r)); eerr == nil {
		summary["granted"] = ent.Source == services.EntitlementSourceGrant || ent.Source == services.EntitlementSourceBoth
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(summary)
}

// SetBillingConsent POST /api/store/billing-consent - session-authed.
// Body {traffic, backup}; an omitted field is left alone.
//
// There is deliberately no uuid in that body. The one this acts on is the one
// the session authenticated, which is the whole reason this endpoint can exist
// in front of a shared-key channel that would otherwise let any caller name any
// account.
func (h *StoreHandler) SetBillingConsent(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		handlers.SendJSONError(w, "The store integration is not configured", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		handlers.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Traffic *bool `json:"traffic"`
		Backup  *bool `json:"backup"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid input", http.StatusBadRequest)
		return
	}
	if req.Traffic == nil && req.Backup == nil {
		handlers.SendJSONError(w, "Nothing to change", http.StatusBadRequest)
		return
	}

	status, message, err := h.state.StoreSetBillingConsent(r.Context(), userID, req.Traffic, req.Backup)
	if err != nil {
		log.Printf("store: billing consent for %s: %v", userID, err)
		handlers.SendJSONError(w, "The store could not be reached, so nothing was changed.", http.StatusBadGateway)
		return
	}
	if status != http.StatusOK {
		// The store's own words. It knows why - no subscription, no meter
		// configured, not billed through Stripe - and each of those sends the
		// reader somewhere different.
		handlers.SendJSONError(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
