package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

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

// storeAccountTimeout bounds the round trip. The panel renders the rest of the
// page without this, so a slow storefront must degrade to "we could not ask"
// rather than hold the whole view.
const storeAccountTimeout = 8 * time.Second

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
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	summary, err := h.state.storeAccountSummary(r.Context(), userID)
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
	if ent, eerr := services.EffectiveEntitlement(h.state.Store, userID, time.Now(), h.state.StoreEnabled, IsAdmin(r)); eerr == nil {
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
		sendJSONError(w, "The store integration is not configured", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Traffic *bool `json:"traffic"`
		Backup  *bool `json:"backup"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		sendJSONError(w, "Invalid input", http.StatusBadRequest)
		return
	}
	if req.Traffic == nil && req.Backup == nil {
		sendJSONError(w, "Nothing to change", http.StatusBadRequest)
		return
	}

	status, message, err := h.state.storeSetBillingConsent(r.Context(), userID, req.Traffic, req.Backup)
	if err != nil {
		log.Printf("store: billing consent for %s: %v", userID, err)
		sendJSONError(w, "The store could not be reached, so nothing was changed.", http.StatusBadGateway)
		return
	}
	if status != http.StatusOK {
		// The store's own words. It knows why - no subscription, no meter
		// configured, not billed through Stripe - and each of those sends the
		// reader somewhere different.
		sendJSONError(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// storeAccountSummary fetches the tenant's picture from the storefront.
func (s *AppState) storeAccountSummary(ctx context.Context, uuid string) (map[string]interface{}, error) {
	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/account-summary?uuid=" + url.QueryEscape(uuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Store-Key", s.StoreSharedKey)

	resp, err := (&http.Client{Timeout: storeAccountTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the storefront refused our key (HTTP %d) - STORE_SHARED_KEY does not match the value configured on %s", resp.StatusCode, s.StoreURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the storefront answered HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("the storefront answered something that is not the expected JSON: %w", err)
	}
	return parsed, nil
}

// storeSetBillingConsent moves one or both switches for this tenant.
//
// It returns the store's status and message rather than collapsing them: a 409
// "no active subscription", a 503 "no meter configured" and a 502 "the payment
// provider said no" are three different people's problem, and one generic
// failure would send all three to the wrong place.
// probeBillingConsentRoute reports whether the storefront's billing-consent
// endpoint is REACHABLE, without changing anything.
//
// It is the third channel Core depends on, and it was the one the health check
// left out. The reason given was that it is a write and no request to it
// changes nothing - but the key is checked before anything else, so a request
// that deliberately omits it is refused on the first line and writes nothing.
//
// The key is left off for a second reason, and it is the one that makes this
// work at all: a missing proxy route on the website answers 404, and so does
// the store itself for a UUID nobody owns. With a valid key those two are
// indistinguishable by status, which is why a "real" probe could not tell a
// broken route from a healthy one. Without a key only the store can answer
// 401, so a 401 proves both that the route exists and that the store is what
// is behind it.
//
// This is what went wrong before: on production only link-status was routed,
// and this component read "up" while every customer's billing page said the
// store could not be reached and nobody could switch metered billing on.
func (s *AppState) probeBillingConsentRoute(ctx context.Context) error {
	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/billing-consent"
	body, _ := json.Marshal(map[string]interface{}{"uuid": healthProbeUUID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// No X-Store-Key on purpose. See above.

	resp, err := (&http.Client{Timeout: storeAccountTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("the storefront answered HTTP %d where the store answers 401, so this route does not reach it", resp.StatusCode)
	}
	return nil
}

func (s *AppState) storeSetBillingConsent(ctx context.Context, uuid string, traffic, backup *bool) (int, string, error) {
	payload := map[string]interface{}{"uuid": uuid}
	if traffic != nil {
		payload["traffic"] = *traffic
	}
	if backup != nil {
		payload["backup"] = *backup
	}
	body, _ := json.Marshal(payload)

	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/billing-consent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Store-Key", s.StoreSharedKey)

	resp, err := (&http.Client{Timeout: storeAccountTimeout}).Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return http.StatusOK, "", nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if parsed.Error == "" {
		parsed.Error = fmt.Sprintf("The store refused the change (HTTP %d).", resp.StatusCode)
	}
	return resp.StatusCode, parsed.Error, nil
}
