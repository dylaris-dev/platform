package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// CustomDomainHandler serves a tenant's view of their own domain claims, and
// the self-service way back from a permanent block.
type CustomDomainHandler struct {
	state    *AppState
	resolver services.DomainResolver
}

func NewCustomDomainHandler(state *AppState, res services.DomainResolver) *CustomDomainHandler {
	return &CustomDomainHandler{state: state, resolver: res}
}

type customDomainView struct {
	Domain     string     `json:"domain"`
	State      string     `json:"state"`
	Attempts   int        `json:"attempts"`
	DeadlineAt *time.Time `json:"deadlineAt,omitempty"`
	// TXTName/TXTValue are the record that proves ownership, shown until it is
	// proven - they are the instruction, not a secret to hide.
	TXTName  string `json:"txtName,omitempty"`
	TXTValue string `json:"txtValue,omitempty"`
}

func viewOf(c store.CustomDomainClaim) customDomainView {
	v := customDomainView{
		Domain: c.Domain, State: c.State, Attempts: c.Attempts, DeadlineAt: c.DeadlineAt,
	}
	if c.State != store.ClaimVerified && c.TXTToken != "" {
		v.TXTName = services.TXTVerifyPrefix + c.Domain
		v.TXTValue = c.TXTToken
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
		sendJSONError(w, "Could not load your custom domains", http.StatusInternalServerError)
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
		sendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	if claim.State == store.ClaimVerified {
		sendJSONError(w, "That domain is already verified", http.StatusBadRequest)
		return
	}
	// Reuse an existing token. Re-minting would invalidate a record the customer
	// may already have published and be waiting on propagation for.
	if claim.TXTToken == "" {
		token, terr := services.NewTXTToken()
		if terr != nil {
			sendJSONError(w, "Could not generate a verification token", http.StatusInternalServerError)
			return
		}
		if serr := h.state.Store.SetCustomDomainTXTToken(claim.ID, token); serr != nil {
			sendJSONError(w, "Could not store the verification token", http.StatusInternalServerError)
			return
		}
		claim.TXTToken = token
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": viewOf(*claim)})
}

// VerifyTXT POST /api/gateway/custom-domains/{domain}/verify-txt
//
// Checks the published record now instead of at the next pass, and on success
// verifies the claim - from pending, or out of a block.
func (h *CustomDomainHandler) VerifyTXT(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	domain := strings.ToLower(strings.TrimSpace(mux.Vars(r)["domain"]))

	claim, err := h.state.Store.GetCustomDomainClaim(userID, domain)
	if err != nil {
		sendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	if claim.State == store.ClaimVerified {
		sendJSONError(w, "That domain is already verified", http.StatusBadRequest)
		return
	}
	if claim.TXTToken == "" {
		sendJSONError(w, "Request a verification token for this domain first", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if !services.CheckTXTToken(ctx, h.resolver, domain, claim.TXTToken) {
		sendJSONError(w,
			"The TXT record was not found yet. DNS changes can take a few minutes to publish.",
			http.StatusConflict)
		return
	}
	if err := h.state.Store.MarkCustomDomainVerified(claim.ID); err != nil {
		sendJSONError(w, "Could not record the verification", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Ownership verified. You can add routes on " + domain + " again.",
	})
}
