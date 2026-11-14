package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// CustomDomainHandler serves a tenant's view of their own domain claims, and
// the self-service way back from a permanent block.
type CustomDomainHandler struct {
	d        Deps
	resolver services.DomainResolver
	// remoteChecks spaces out a user's checks on the managed gateway, where
	// each one may cost a call on the platform's one provider key.
	remoteChecks *httpx.IPRateLimiter
}

func NewCustomDomainHandler(d Deps, res services.DomainResolver) *CustomDomainHandler {
	return &CustomDomainHandler{d: d, resolver: res, remoteChecks: httpx.NewIPRateLimiter()}
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

// providerClaimView is the provider's claim on a domain, as the managed
// gateway shows it beside the local claim. State is the provider's, or
// "missing" while the provider holds no claim on the domain.
type providerClaimView struct {
	State    string     `json:"state"`
	TXTName  string     `json:"txtName,omitempty"`
	TXTValue string     `json:"txtValue,omitempty"`
	LapsesAt *time.Time `json:"lapsesAt,omitempty"`
}

// remoteDomainView is a local claim on the managed gateway: the record that
// proves the domain to this platform, plus the provider's (nil: unknown, the
// provider could not be read).
type remoteDomainView struct {
	customDomainView
	Provider *providerClaimView `json:"provider"`
}

// providerViewOf keeps State to a closed set: pending, verified, blocked,
// permablocked, missing. A state the provider might add later reads as pending
// (not verified, so no route), never as a value the panel does not know.
func providerViewOf(p remotegw.CustomDomain, found bool) *providerClaimView {
	if !found {
		return &providerClaimView{State: "missing"}
	}
	state := p.State
	switch state {
	case store.ClaimPending, store.ClaimVerified, store.ClaimBlocked, store.ClaimPermablocked:
	default:
		state = store.ClaimPending
	}
	return &providerClaimView{State: state, TXTName: p.TXTName, TXTValue: p.TXTValue, LapsesAt: p.LapsesAt}
}

// providerErrorView is a provider failure inside an answer that still carries
// the local claims. Its message never comes from a provider answer that could
// name a TXT value (gate.ProviderErrorAnswer replaces those).
func providerErrorView(err error) map[string]string {
	_, code, msg, ok := gate.ProviderErrorAnswer(err)
	if !ok {
		code, msg = "provider_error", "The gateway provider could not be asked. Try again in a moment."
	}
	return map[string]string{"code": code, "message": msg}
}

// providerCode is a provider failure for a log line: the code only, as the
// provider's own message may name its TXT value.
func providerCode(err error) string {
	if _, code, _, ok := gate.ProviderErrorAnswer(err); ok {
		return code
	}
	return "local error"
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
//
// On the managed gateway each claim also carries the provider's claim on that
// domain. The provider's list is the whole platform's, so only the caller's own
// domains are taken from it.
func (h *CustomDomainHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	if rg != nil && !h.remoteChecks.Allow("list:user:"+userID, remoteListsPerMin) {
		w.Header().Set("Retry-After", "60")
		httpx.SendJSONErrorCode(w, "Too many requests, try again in a minute.", "rate_limited", http.StatusTooManyRequests)
		return
	}
	claims, err := h.d.Store.ListCustomDomainClaimsByUser(userID)
	if err != nil {
		httpx.SendJSONError(w, "Could not load your custom domains", http.StatusInternalServerError)
		return
	}
	if rg != nil {
		h.listRemote(w, r, rg, claims)
		return
	}
	out := make([]customDomainView, 0, len(claims))
	for _, c := range claims {
		out = append(out, viewOf(c))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domains": out})
}

// listRemote answers List on the managed gateway. The local claims are listed
// even when the provider cannot be read: then each provider is null and
// providerError says why.
func (h *CustomDomainHandler) listRemote(w http.ResponseWriter, r *http.Request, rg *services.RemoteGateway, claims []store.CustomDomainClaim) {
	out := map[string]interface{}{"success": true}
	provider, perr := rg.ProviderCustomDomains(r.Context())
	if perr != nil {
		log.Printf("custom domains: provider claims not read: %s", providerCode(perr))
		out["providerError"] = providerErrorView(perr)
	}
	views := make([]remoteDomainView, 0, len(claims))
	for _, c := range claims {
		v := remoteDomainView{customDomainView: viewOf(c)}
		if perr == nil {
			p, ok := provider[c.Domain]
			v.Provider = providerViewOf(p, ok)
		}
		views = append(views, v)
	}
	out["domains"] = views
	if limit, err := h.ownDomainLimit(r.Context(), rg); err == nil {
		out["limit"] = limit
	} else if out["providerError"] == nil {
		log.Printf("custom domains: own-domain limit not read: %s", providerCode(err))
		out["providerError"] = providerErrorView(err)
	}
	json.NewEncoder(w).Encode(out)
}

// ownDomainLimit is the provider plan's cap on own-domain addresses (nil: no
// cap) and how many of them this platform holds, over every user.
func (h *CustomDomainHandler) ownDomainLimit(ctx context.Context, rg *services.RemoteGateway) (map[string]interface{}, error) {
	acct, err := rg.Account(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := h.d.Store.ListRemoteRoutes()
	if err != nil {
		return nil, err
	}
	// A row whose delete is queued is on its way out (as ServerDomains).
	deleting, err := h.d.Store.OpenOutboxDomains(services.OutboxOpDelete)
	if err != nil {
		return nil, err
	}
	used := 0
	for _, row := range rows {
		if row.CustomDomain && !deleting[row.Domain] {
			used++
		}
	}
	return map[string]interface{}{"maxCustomDomains": acct.Limits.MaxCustomDomains, "used": used}, nil
}

// IssueTXTToken POST /api/gateway/custom-domains/{domain}/txt-token
//
// Mints (once) the record that proves ownership. A claim gets its token when it
// is armed; this is for a claim that predates that, and the way back from a
// permanent block.
func (h *CustomDomainHandler) IssueTXTToken(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	domain := strings.ToLower(strings.TrimSpace(mux.Vars(r)["domain"]))
	// Only the local record is minted here, in either mode; an unread mode is
	// still answered first, as by the other claim handlers.
	if err := services.LocalGatewayErr(h.d.GatewayRemote); errors.Is(err, services.ErrGatewayProviderUnknown) {
		gate.GatewayModeError(w, err)
		return
	}

	claim, err := h.d.Store.GetCustomDomainClaim(userID, domain)
	if err != nil {
		httpx.SendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	if claim.State == store.ClaimVerified {
		httpx.SendJSONError(w, "That domain is already verified", http.StatusBadRequest)
		return
	}
	// Reuse an existing token. Re-minting would invalidate a record the customer
	// may already have published and be waiting on propagation for.
	if claim.TXTToken == "" {
		token, terr := services.NewTXTToken()
		if terr != nil {
			httpx.SendJSONError(w, "Could not generate a verification token", http.StatusInternalServerError)
			return
		}
		if serr := h.d.Store.SetCustomDomainTXTToken(claim.ID, token); serr != nil {
			httpx.SendJSONError(w, "Could not store the verification token", http.StatusInternalServerError)
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
//
// On the managed gateway it checks both records, this platform's and the
// provider's (verifyRemote).
func (h *CustomDomainHandler) VerifyTXT(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	domain := strings.ToLower(strings.TrimSpace(mux.Vars(r)["domain"]))
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	if rg != nil {
		h.verifyRemote(w, r, rg, userID, domain)
		return
	}

	claim, err := h.d.Store.GetCustomDomainClaim(userID, domain)
	if err != nil {
		httpx.SendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	if claim.State == store.ClaimVerified && claim.FailingSince == nil {
		httpx.SendJSONError(w, "That domain is already verified", http.StatusBadRequest)
		return
	}
	if claim.TXTToken == "" {
		httpx.SendJSONError(w, "Request a verification token for this domain first", http.StatusBadRequest)
		return
	}
	if wait := h.verifyCooldown(r.Context(), userID, domain); wait > 0 {
		httpx.SendJSONErrorCode(w, fmt.Sprintf("You checked this domain a moment ago. Try again in %s.", roundUpMinutes(wait)),
			"verify_cooldown", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if !services.CheckTXTToken(ctx, h.resolver, domain, claim.TXTToken) {
		httpx.SendJSONError(w,
			"The TXT record was not found yet. DNS changes can take a few minutes to publish.",
			http.StatusConflict)
		return
	}
	if err := h.d.Store.MarkCustomDomainVerified(claim.ID); err != nil {
		httpx.SendJSONError(w, "Could not record the verification", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Ownership verified. You can add routes on " + domain + ".",
	})
}

// remoteChecksPerMin bounds one user's own-domain changes on the managed
// gateway: adds, checks and deletes share one bucket, since each may cost a
// write on the platform's one provider key.
const remoteChecksPerMin = 10

// remoteListsPerMin bounds one user's own-domain lists on the managed gateway.
const remoteListsPerMin = 60

// allowRemoteChange takes one of the user's remoteChecksPerMin, or answers 429.
func (h *CustomDomainHandler) allowRemoteChange(w http.ResponseWriter, userID string) bool {
	if h.remoteChecks.Allow("user:"+userID, remoteChecksPerMin) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	httpx.SendJSONErrorCode(w, "Too many requests, try again in a minute.", "rate_limited", http.StatusTooManyRequests)
	return false
}

// maxPendingProviderClaims bounds a user's own domains that the provider has
// not verified yet. Every one of them holds a claim in the provider's bound,
// which is the whole platform's, before any DNS proof.
const maxPendingProviderClaims = 3

// maxOpenProviderReleases bounds the provider claim releases still queued on
// the whole platform before a new domain is added: the queue does not record
// which user caused one, and add-delete-add would otherwise stack claims on the
// provider faster than the releases drain.
const maxOpenProviderReleases = 10

// verifyRemote is VerifyTXT on the managed gateway: this platform's record is
// checked as on the local path (and its cooldown applies to the call), then
// the provider is asked to check its own, unless it already has it. A
// provider that has no claim on the domain gets one (the way back after a
// failed Add or a release). The provider's refusals never fail the local
// verification; the answer says per side what is still missing:
// {success, local, provider, message, providerError?}, 200 when a side was
// verified by this call or both already are, else 409.
func (h *CustomDomainHandler) verifyRemote(w http.ResponseWriter, r *http.Request, rg *services.RemoteGateway, userID, domain string) {
	if !h.allowRemoteChange(w, userID) {
		return
	}
	claim, err := h.d.Store.GetCustomDomainClaim(userID, domain)
	if err != nil {
		httpx.SendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	localDone := claim.State == store.ClaimVerified && claim.FailingSince == nil
	provider, perr := rg.ProviderCustomDomains(r.Context())
	pc, known := provider[domain]
	providerDone := perr == nil && known && pc.State == store.ClaimVerified && pc.LapsesAt == nil
	if perr == nil && !providerDone {
		// The cached list is this replica's and up to a minute old: a claim
		// added or verified through another replica is not in it yet. Decided
		// on a fresh read, so the check below asks about the claim that is.
		provider, perr = rg.FreshProviderCustomDomains(r.Context())
		pc, known = provider[domain]
		providerDone = perr == nil && known && pc.State == store.ClaimVerified && pc.LapsesAt == nil
	}
	if !localDone || !providerDone {
		if !localDone && claim.TXTToken == "" {
			httpx.SendJSONError(w, "Request a verification token for this domain first", http.StatusBadRequest)
			return
		}
		if wait := h.verifyCooldown(r.Context(), userID, domain); wait > 0 {
			httpx.SendJSONErrorCode(w, fmt.Sprintf("You checked this domain a moment ago. Try again in %s.", roundUpMinutes(wait)),
				"verify_cooldown", http.StatusTooManyRequests)
			return
		}
	}

	advanced := false
	if !localDone {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		localDone = services.CheckTXTToken(ctx, h.resolver, domain, claim.TXTToken)
		cancel()
		if localDone {
			if err := h.d.Store.MarkCustomDomainVerified(claim.ID); err != nil {
				httpx.SendJSONError(w, "Could not record the verification", http.StatusInternalServerError)
				return
			}
			advanced = true
		}
	}

	out := map[string]interface{}{}
	cooldown := false
	// providerFailed: the provider could not be asked, which is not "its record
	// was not found"; the answer says what went wrong instead.
	providerFailed := ""
	if !providerDone {
		var err error
		// A claim made (or found) here is checked in the same call: "check
		// now" asks the provider, whatever the list said before.
		ensureThenVerify := func() {
			if _, err = rg.EnsureProviderClaim(r.Context(), domain); err == nil {
				providerDone, err = rg.VerifyProviderClaim(r.Context(), domain)
			}
		}
		switch {
		case perr != nil:
			// Without the list nothing is known about the provider's claim.
			err = perr
		case !known:
			ensureThenVerify()
		default:
			if providerDone, err = rg.VerifyProviderClaim(r.Context(), domain); remotegw.IsCode(err, "not_found") {
				// The provider's claim went (a release); a new one has a new value.
				ensureThenVerify()
			}
		}
		switch {
		case providerDone:
			advanced = true
		case remotegw.IsCode(err, "verify_cooldown"):
			cooldown = true
		case err != nil:
			log.Printf("custom domain %s: provider check: %s", domain, providerCode(err))
			pe := providerErrorView(err)
			out["providerError"] = pe
			providerFailed = pe["message"]
		}
	}

	state := func(done bool) string {
		if done {
			return store.ClaimVerified
		}
		return store.ClaimPending
	}
	const later = "DNS changes can take a few minutes to publish."
	var msg string
	switch {
	case localDone && providerDone:
		msg = "Ownership verified. You can add routes on " + domain + "."
	case localDone && cooldown:
		msg = "This platform has verified " + domain + ". The gateway provider checked it a moment ago: try again in a few minutes."
	case localDone && providerFailed != "":
		msg = "This platform has verified " + domain + ". The gateway provider could not check it: " + providerFailed
	case providerFailed != "":
		msg = "The TXT record for this platform was not found yet. " + later + " The gateway provider could not check it: " + providerFailed
	case localDone:
		msg = "This platform has verified " + domain + ". The gateway provider's TXT record was not found yet. " + later
	case providerDone:
		msg = "The gateway provider has verified " + domain + ". The TXT record for this platform was not found yet. " + later
	default:
		msg = "Neither TXT record was found yet. " + later
	}
	if cooldown {
		// The panel tells this case by the flag, not by the message text.
		out["providerCooldown"] = true
	}
	ok := advanced || (localDone && providerDone)
	out["success"], out["local"], out["provider"], out["message"] = ok, state(localDone), state(providerDone), msg
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
	}
	json.NewEncoder(w).Encode(out)
}

// providerMayHoldClaim is false for an Add the provider refused outright (or
// that never reached it), so no release is queued for a claim that was never
// made: a queued release would answer the next Add with change_pending.
func providerMayHoldClaim(err error) bool {
	if errors.Is(err, services.ErrGatewayChangePending) || errors.Is(err, services.ErrOwnDomainWritesBusy) {
		return false
	}
	for _, code := range []string{"too_many_domains", "custom_domains_off", "bad_request", "unauthorized"} {
		if remotegw.IsCode(err, code) {
			return false
		}
	}
	return true
}

// verifyCheckCooldown spaces out manual checks of one domain. A check only
// looks the record up: nothing counts a miss, so no number of clicks can block
// a domain, and the verifier still checks pending claims on its own.
const verifyCheckCooldown = 5 * time.Minute

// verifyCooldown claims this (user, domain)'s check slot and returns how long
// is left when it was already taken. No Redis means no cooldown.
func (h *CustomDomainHandler) verifyCooldown(ctx context.Context, userID, domain string) time.Duration {
	if h.d.Redis == nil {
		return 0
	}
	key := "dylaris:custom-domain:check:" + userID + ":" + domain
	ok, err := h.d.Redis.SetNX(ctx, key, "1", verifyCheckCooldown).Result()
	if err != nil || ok {
		return 0
	}
	ttl, err := h.d.Redis.TTL(ctx, key).Result()
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
//
// On the managed gateway the provider gets its claim too, AFTER the local one
// exists: a release running meanwhile sees the local claim and keeps the
// provider's. A provider refusal takes back a local claim made by this call.
// A user may hold at most maxPendingProviderClaims domains the provider has not
// verified, and add 10 a minute.
func (h *CustomDomainHandler) Add(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	// The mode first, as the gate in front of this route answered before.
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		httpx.SendJSONError(w, `Invalid request: expected {"domain": "play.example.com"}`, http.StatusBadRequest)
		return
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Domain), "."))
	var hosters []services.HosterDomain
	if rg == nil {
		raw, _ := h.d.Store.GetSetting("gateway_hoster_domains")
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &hosters)
		}
	}
	enabled, _ := h.d.Store.GetSetting("gateway_custom_domains_enabled")
	if enabled != "true" && !reqctx.IsAdmin(r) {
		httpx.SendJSONErrorCode(w, "Your own domains are not enabled on this platform.", "custom_domains_off", http.StatusConflict)
		return
	}
	if rg != nil {
		if !h.allowRemoteChange(w, userID) {
			return
		}
		// No route on an own domain is possible at a cap of none, so its claims
		// would only take slots of the platform's provider bound.
		if limit, _ := customDomainCapFor(h.d.Store, userID); limit != nil && *limit == 0 && !reqctx.IsAdmin(r) {
			httpx.SendJSONErrorCode(w, "Your account cannot use own domains.", "custom_domain_cap", http.StatusForbidden)
			return
		}
		regions, err := rg.Regions(r.Context())
		if gate.GatewayProviderError(w, err) {
			return
		}
		if err != nil {
			httpx.SendJSONErrorCode(w, "Could not load the address options. Try again.", "provider_error", http.StatusInternalServerError)
			return
		}
		if !regions.CustomDomainsEnabled {
			gate.WriteGatewayError(w, http.StatusConflict, "custom_domains_off", "Own domains are not enabled for this account on the managed gateway.")
			return
		}
		hosters = providerHosters(regions)
	}
	if _, _, err := resolveBroughtDomain(domain, hosters, true, nil); err != nil {
		httpx.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	claims, err := h.d.Store.ListCustomDomainClaimsByUser(userID)
	if err != nil {
		httpx.SendJSONError(w, "Could not load your custom domains", http.StatusInternalServerError)
		return
	}
	known := false
	for _, c := range claims {
		known = known || c.Domain == domain
	}
	// A scoped cap above the claim bound would otherwise be unreachable (the
	// claim must exist before a route on it can), and an account set to no cap
	// is not bounded by the constant either.
	claimCap := int64(maxClaimsPerUser)
	unboundedClaims := false
	if scopedCap, scoped := customDomainCapFor(h.d.Store, userID); scoped {
		if scopedCap == nil {
			unboundedClaims = true
		} else if *scopedCap > claimCap {
			claimCap = *scopedCap
		}
	}
	if !known && !unboundedClaims && int64(len(claims)) >= claimCap {
		httpx.SendJSONErrorCode(w, fmt.Sprintf("You can add up to %d domains.", claimCap), "too_many_domains", http.StatusConflict)
		return
	}
	if rg != nil && !known {
		provider, err := rg.ProviderCustomDomains(r.Context())
		if gate.GatewayProviderError(w, err) {
			return
		}
		if err != nil {
			httpx.SendJSONErrorCode(w, "Could not load your custom domains", "provider_error", http.StatusInternalServerError)
			return
		}
		releasing, err := h.d.Store.OpenOutboxDomains(services.OutboxOpDeleteCustomDomain)
		if err != nil {
			httpx.SendJSONErrorCode(w, "Could not load your custom domains", "internal", http.StatusInternalServerError)
			return
		}
		if len(releasing) >= maxOpenProviderReleases {
			httpx.SendJSONErrorCode(w, "Removed domains of this platform are still being released at the gateway provider. Try again in a few minutes.",
				"pending_domains_limit", http.StatusConflict)
			return
		}
		pending := 0
		for _, c := range claims {
			if p, ok := provider[c.Domain]; !ok || p.State != store.ClaimVerified {
				pending++
			}
		}
		if pending >= maxPendingProviderClaims {
			httpx.SendJSONErrorCode(w, "Finish verifying your pending domains before adding another.", "pending_domains_limit", http.StatusConflict)
			return
		}
	}
	var claim *store.CustomDomainClaim
	created := false
	if rg != nil {
		claim, created, err = h.d.Store.RequestCustomDomainClaimCreated(userID, domain)
	} else {
		claim, err = h.d.Store.RequestCustomDomainClaim(userID, domain)
	}
	if err != nil || claim == nil {
		httpx.SendJSONError(w, "Could not add the domain", http.StatusInternalServerError)
		return
	}
	if claim.TXTToken == "" && claim.State != store.ClaimVerified {
		token, terr := services.NewTXTToken()
		if terr == nil {
			terr = h.d.Store.SetCustomDomainTXTToken(claim.ID, token)
		}
		if terr != nil {
			httpx.SendJSONError(w, "Could not generate a verification token", http.StatusInternalServerError)
			return
		}
		claim.TXTToken = token
	}
	if rg == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": viewOf(*claim)})
		return
	}
	pc, err := rg.EnsureProviderClaim(r.Context(), domain)
	if err != nil {
		if created {
			if _, derr := h.d.Store.DeleteCustomDomainClaim(userID, domain); derr != nil {
				log.Printf("custom domain %s (user %s): claim not taken back after the provider refused: %v", domain, userID, derr)
			} else if providerMayHoldClaim(err) {
				// The provider may have made its claim before a later step failed
				// (its TXT value, a lost answer): released unless someone needs it.
				if rerr := rg.ReleaseProviderClaim(domain, userID); rerr != nil {
					log.Printf("custom domain %s (user %s): provider claim release not queued: %v", domain, userID, rerr)
				}
			}
		}
		if !gate.GatewayProviderError(w, err) {
			log.Printf("custom domain %s: provider claim: %v", domain, err)
			httpx.SendJSONError(w, "Could not add the domain", http.StatusInternalServerError)
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
		"domain": remoteDomainView{customDomainView: viewOf(*claim), Provider: providerViewOf(pc, true)}})
}

// Delete DELETE /api/gateway/custom-domains/{domain} - removes the caller's
// claim on a domain. Refused while a route of theirs still uses it: the claim
// is what keeps that route checked, and removing it first would leave a
// route nobody re-verifies.
//
// On the managed gateway the routes are the local remote_routes rows of the
// caller's servers (one row per domain; a row whose delete is queued is on
// its way out), and the provider's claim is released afterwards unless
// another user of this platform still holds the domain.
func (h *CustomDomainHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	domain := strings.ToLower(strings.TrimSpace(mux.Vars(r)["domain"]))
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	// Each delete may queue a provider write.
	if rg != nil && !h.allowRemoteChange(w, userID) {
		return
	}
	owned, err := services.OwnedServerUUIDs(h.d.Store, userID)
	if err != nil {
		httpx.SendJSONError(w, "Could not check your routes", http.StatusInternalServerError)
		return
	}
	if rg != nil {
		row, err := h.d.Store.GetRemoteRoute(domain)
		deleting := false
		if err == nil && row != nil && owned[row.ServerUUID] {
			deleting, err = h.d.Store.HasOpenOutbox(domain, services.OutboxOpDelete)
		}
		if err != nil {
			httpx.SendJSONError(w, "Could not check your routes", http.StatusInternalServerError)
			return
		}
		if row != nil && owned[row.ServerUUID] && !deleting {
			httpx.SendJSONErrorCode(w, "Delete the routes on "+domain+" first.", "domain_in_use", http.StatusConflict)
			return
		}
	} else {
		routes, err := services.ReadRoutesFromRedis(r.Context(), h.d.Redis)
		if err != nil {
			// Not knowing the routes is not "there are none".
			httpx.SendJSONError(w, "Could not check your routes", http.StatusServiceUnavailable)
			return
		}
		for _, rt := range routes {
			if strings.EqualFold(rt.Domain, domain) && services.RouteHeldBy(rt, userID, owned) {
				httpx.SendJSONErrorCode(w, "Delete the routes on "+domain+" first.", "domain_in_use", http.StatusConflict)
				return
			}
		}
	}
	found, err := h.d.Store.DeleteCustomDomainClaim(userID, domain)
	if err != nil {
		httpx.SendJSONError(w, "Could not remove the domain", http.StatusInternalServerError)
		return
	}
	if !found {
		httpx.SendJSONError(w, "No claim on that domain for your account", http.StatusNotFound)
		return
	}
	if rg != nil {
		// The local claim is gone either way; a release not queued leaves the
		// provider's claim in place, which the next Add of the domain reuses.
		if err := rg.ReleaseProviderClaim(domain, userID); err != nil {
			log.Printf("custom domain %s (user %s): provider claim release not queued: %v", domain, userID, err)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
