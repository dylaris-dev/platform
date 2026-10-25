package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// CustomDomainHandler serves a tenant's view of their own domain claims, and
// the self-service way back from a permanent block.
type CustomDomainHandler struct {
	state    *handlers.AppState
	resolver services.DomainResolver
}

func NewCustomDomainHandler(state *handlers.AppState, res services.DomainResolver) *CustomDomainHandler {
	return &CustomDomainHandler{state: state, resolver: res}
}

type customDomainView struct {
	Domain     string     `json:"domain"`
	State      string     `json:"state"`
	Attempts   int        `json:"attempts"`
	DeadlineAt *time.Time `json:"deadlineAt,omitempty"`
	// TXTName/TXTValue are the record that proves ownership - they are the
	// instruction, not a secret to hide. Shown for a verified claim too: it is
	// re-checked, so the record has to stay published.
	TXTName  string `json:"txtName,omitempty"`
	TXTValue string `json:"txtValue,omitempty"`
	// LapsesAt is set while a verified claim's record is missing: when its
	// routes are removed unless the record comes back.
	LapsesAt *time.Time `json:"lapsesAt,omitempty"`
}

func viewOf(c store.CustomDomainClaim) customDomainView {
	v := customDomainView{
		Domain: c.Domain, State: c.State, Attempts: c.Attempts, DeadlineAt: c.DeadlineAt,
	}
	if c.TXTToken != "" {
		v.TXTName = services.TXTVerifyPrefix + c.Domain
		v.TXTValue = c.TXTToken
	}
	if c.State == store.ClaimVerified && c.FailingSince != nil {
		at := c.FailingSince.Add(services.CustomDomainLapseGrace)
		v.LapsesAt = &at
	}
	return v
}

// List GET /api/gateway/custom-domains - the caller's OWN claims.
//
// Scoped to the caller inside the handler rather than by a capability: a claim
// is a fact about one user's relationship to one domain, and there is no reason
// for a tenant to see another tenant's.
func (h *CustomDomainHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	claims, err := h.state.Store.ListCustomDomainClaimsByUser(userID)
	if err != nil {
		handlers.SendJSONError(w, "Could not load your custom domains", http.StatusInternalServerError)
		return
	}
	out := make([]customDomainView, 0, len(claims))
	for _, c := range claims {
		out = append(out, viewOf(c))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domains": out})
}

// IssueTXTToken POST /api/gateway/custom-domains/{domain}/txt-token
//
// Mints (once) the record that proves ownership. A claim gets its token when it
// is armed; this is for a claim that predates that, and the way back from a
// permanent block.
func (h *CustomDomainHandler) IssueTXTToken(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	domain := strings.ToLower(strings.TrimSpace(mux.Vars(r)["domain"]))

	claim, err := h.state.Store.GetCustomDomainClaim(userID, domain)
	if err != nil {
		handlers.SendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	if claim.State == store.ClaimVerified {
		handlers.SendJSONError(w, "That domain is already verified", http.StatusBadRequest)
		return
	}
	// Reuse an existing token. Re-minting would invalidate a record the customer
	// may already have published and be waiting on propagation for.
	if claim.TXTToken == "" {
		token, terr := services.NewTXTToken()
		if terr != nil {
			handlers.SendJSONError(w, "Could not generate a verification token", http.StatusInternalServerError)
			return
		}
		if serr := h.state.Store.SetCustomDomainTXTToken(claim.ID, token); serr != nil {
			handlers.SendJSONError(w, "Could not store the verification token", http.StatusInternalServerError)
			return
		}
		claim.TXTToken = token
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": viewOf(*claim)})
}

// VerifyTXT POST /api/gateway/custom-domains/{domain}/verify-txt
//
// Checks the published record now instead of at the next pass, and on success
// verifies the claim - from pending, out of a block, or out of a run of missed
// re-checks.
func (h *CustomDomainHandler) VerifyTXT(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	domain := strings.ToLower(strings.TrimSpace(mux.Vars(r)["domain"]))

	claim, err := h.state.Store.GetCustomDomainClaim(userID, domain)
	if err != nil {
		handlers.SendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	if claim.State == store.ClaimVerified && claim.FailingSince == nil {
		handlers.SendJSONError(w, "That domain is already verified", http.StatusBadRequest)
		return
	}
	if claim.TXTToken == "" {
		handlers.SendJSONError(w, "Request a verification token for this domain first", http.StatusBadRequest)
		return
	}
	if wait := h.verifyCooldown(r.Context(), userID, domain); wait > 0 {
		handlers.SendJSONErrorCode(w, fmt.Sprintf("You checked this domain a moment ago. Try again in %s.", roundUpMinutes(wait)),
			"verify_cooldown", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if !services.CheckTXTToken(ctx, h.resolver, domain, claim.TXTToken) {
		handlers.SendJSONError(w,
			"The TXT record was not found yet. DNS changes can take a few minutes to publish.",
			http.StatusConflict)
		return
	}
	if err := h.state.Store.MarkCustomDomainVerified(claim.ID); err != nil {
		handlers.SendJSONError(w, "Could not record the verification", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Ownership verified. You can add routes on " + domain + ".",
	})
}

// verifyCheckCooldown spaces out manual checks of one domain. A check only
// looks the record up: nothing counts a miss, so no number of clicks can block
// a domain, and the verifier still checks pending claims on its own.
const verifyCheckCooldown = 5 * time.Minute

// verifyCooldown claims this (user, domain)'s check slot and returns how long
// is left when it was already taken. No Redis means no cooldown.
func (h *CustomDomainHandler) verifyCooldown(ctx context.Context, userID, domain string) time.Duration {
	if h.state.Redis == nil {
		return 0
	}
	key := "dylaris:custom-domain:check:" + userID + ":" + domain
	ok, err := h.state.Redis.SetNX(ctx, key, "1", verifyCheckCooldown).Result()
	if err != nil || ok {
		return 0
	}
	ttl, err := h.state.Redis.TTL(ctx, key).Result()
	if err != nil || ttl <= 0 {
		return 0
	}
	return ttl
}

func roundUpMinutes(d time.Duration) string {
	m := int((d + time.Minute - 1) / time.Minute)
	if m <= 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// maxClaimsPerUser bounds how many domains one account may add, the
// per-account custom domain allowance the docs state.
const maxClaimsPerUser = 50

// Add POST /api/gateway/custom-domains - body {"domain": "play.example.com"}.
//
// The first step for a domain of the customer's own: it records the claim
// (no deadline, see store.RequestCustomDomainClaim) and returns the TXT
// record to publish. Routes on it are refused until that record is found
// (customDomainGate), so adding a domain is never a way to serve it.
func (h *CustomDomainHandler) Add(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		handlers.SendJSONError(w, `Invalid request: expected {"domain": "play.example.com"}`, http.StatusBadRequest)
		return
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Domain), "."))
	raw, _ := h.state.Store.GetSetting("gateway_hoster_domains")
	var hosters []handlers.HosterDomain
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &hosters)
	}
	enabled, _ := h.state.Store.GetSetting("gateway_custom_domains_enabled")
	if enabled != "true" && !handlers.IsAdmin(r) {
		handlers.SendJSONErrorCode(w, "Your own domains are not enabled on this platform.", "custom_domains_off", http.StatusConflict)
		return
	}
	if _, _, err := resolveBroughtDomain(domain, hosters, true, nil); err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	claims, err := h.state.Store.ListCustomDomainClaimsByUser(userID)
	if err != nil {
		handlers.SendJSONError(w, "Could not load your custom domains", http.StatusInternalServerError)
		return
	}
	known := false
	for _, c := range claims {
		known = known || c.Domain == domain
	}
	if !known && len(claims) >= maxClaimsPerUser {
		handlers.SendJSONErrorCode(w, fmt.Sprintf("You can add up to %d domains.", maxClaimsPerUser), "too_many_domains", http.StatusConflict)
		return
	}
	claim, err := h.state.Store.RequestCustomDomainClaim(userID, domain)
	if err != nil || claim == nil {
		handlers.SendJSONError(w, "Could not add the domain", http.StatusInternalServerError)
		return
	}
	if claim.TXTToken == "" && claim.State != store.ClaimVerified {
		token, terr := services.NewTXTToken()
		if terr == nil {
			terr = h.state.Store.SetCustomDomainTXTToken(claim.ID, token)
		}
		if terr != nil {
			handlers.SendJSONError(w, "Could not generate a verification token", http.StatusInternalServerError)
			return
		}
		claim.TXTToken = token
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": viewOf(*claim)})
}
