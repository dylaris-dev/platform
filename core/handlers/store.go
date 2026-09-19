package handlers

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"
)

// Store-linking: the bridge between the platform Core (source of truth for the
// real user) and the dylaris.com storefront (passwordless accounts + billing).
// The two are decoupled; the only join is the Core user UUID, which the store
// holds (single-owned link). Core never stores a copy — it reads "linked" status
// from the store on demand.
//
// Trust model (two layers):
//   - STORE_SHARED_KEY = service-to-service trust (this is my partner store),
//     carried in the X-Store-Key header on store<->core calls. NOT a user proof.
//   - The link-token = user proof. Core mints a single-use, short-TTL token bound
//     to the authenticated panel user's UUID+email, then redirects the browser to
//     the store. The store exchanges the token (over the shared key) for {uuid,
//     email} and asks the user to confirm before persisting the link.

const (
	storeLinkTokenPrefix = "dylaris:store:linktoken:"
	storeLinkTokenTTL    = 10 * time.Minute
)

// StoreHandler serves the core-side store-linking endpoints.
type StoreHandler struct {
	state *AppState
}

func NewStoreHandler(state *AppState) *StoreHandler {
	return &StoreHandler{state: state}
}

// requireStoreKey enforces the service-to-service shared key (constant-time) and
// that the store integration is enabled. Returns true when the request may
// proceed; otherwise it writes the error response and returns false.
func (h *StoreHandler) requireStoreKey(w http.ResponseWriter, r *http.Request) bool {
	if !h.state.StoreEnabled {
		sendJSONError(w, "Store integration not enabled", http.StatusNotFound)
		return false
	}
	got := r.Header.Get("X-Store-Key")
	want := h.state.StoreSharedKey
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		sendJSONError(w, "Invalid store key", http.StatusUnauthorized)
		return false
	}
	return true
}

// LinkStart POST /api/store/link/start — authed panel user. Mints a single-use,
// short-TTL token bound to the caller's UUID+email and returns the storefront
// redirect URL. The browser then lands on dylaris.com/connect?token=...
func (h *StoreHandler) LinkStart(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		sendJSONError(w, "Store integration not enabled", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := h.state.Store.GetUserByID(userID)
	if err != nil || user == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		sendJSONError(w, "Failed to mint token", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(tokenBytes)

	// Bind the token to uuid+email+username. Single-use is enforced by GetDel on
	// verify. Newline-delimited; verify tolerates an absent username (old tokens).
	value := user.ID + "\n" + user.Email + "\n" + user.Username
	if err := h.state.Redis.Set(r.Context(), storeLinkTokenPrefix+token, value, storeLinkTokenTTL).Err(); err != nil {
		sendJSONError(w, "Failed to store token", http.StatusInternalServerError)
		return
	}

	redirectURL := strings.TrimRight(h.state.StoreURL, "/") + "/connect?token=" + url.QueryEscape(token)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "redirectUrl": redirectURL})
}

// LinkVerify POST /api/store/link/verify — store-key. Body {token}. Validates +
// consumes the token (single-use) and returns the bound {uuid, email, username}
// plus this Core's own panel URL. Called by dylaris.com during the connect flow.
//
// panelUrl travels HERE rather than as a query parameter on the /connect link,
// which is the obvious alternative and the wrong one: the storefront sends the
// browser back to it after linking, so a value taken from the URL would be an
// open redirect anyone could aim anywhere. Coming back over the store-key
// channel it is attested by the same Core that minted the token, and there is
// nothing for a visitor to tamper with.
func (h *StoreHandler) LinkVerify(w http.ResponseWriter, r *http.Request) {
	if !h.requireStoreKey(w, r) {
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		sendJSONError(w, "Invalid token", http.StatusBadRequest)
		return
	}

	// GetDel = atomic read-and-consume, so a token can never be redeemed twice.
	value, err := h.state.Redis.GetDel(r.Context(), storeLinkTokenPrefix+req.Token).Result()
	if err != nil || value == "" {
		sendJSONError(w, "Invalid or expired token", http.StatusUnauthorized)
		return
	}
	parts := strings.SplitN(value, "\n", 3)
	if len(parts) < 2 {
		sendJSONError(w, "Malformed token", http.StatusInternalServerError)
		return
	}
	username := ""
	if len(parts) == 3 {
		username = parts[2]
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"uuid":     parts[0],
		"email":    parts[1],
		"username": username,
		// Empty when FRONTEND_URL is unset; the storefront then simply offers no
		// way back and says so, rather than guessing an origin.
		"panelUrl": strings.TrimRight(h.state.FrontendURL, "/"),
	})
}

// VerifyUser GET /api/store/verify-user?uuid= — store-key. The purchase-gate
// doppelcheck: dylaris.com calls this right before charging to confirm the
// linked Core user still exists.
func (h *StoreHandler) VerifyUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireStoreKey(w, r) {
		return
	}
	uuid := strings.TrimSpace(r.URL.Query().Get("uuid"))
	if uuid == "" {
		sendJSONError(w, "Missing uuid", http.StatusBadRequest)
		return
	}
	user, err := h.state.Store.GetUserByID(uuid)
	if err != nil || user == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "exists": false})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"exists":   true,
		"uuid":     user.ID,
		"username": user.Username,
	})
}

// GetUsage GET /api/store/usage?uuid= — store-key. Returns the linked tenant's
// metered traffic for the current billing month so the store can compute overage
// and bill it. edge_bytes is the billable player traffic; the rest are for
// observability. A tenant with no traffic yet returns zeros (never an error).
func (h *StoreHandler) GetUsage(w http.ResponseWriter, r *http.Request) {
	if !h.requireStoreKey(w, r) {
		return
	}
	uuid := strings.TrimSpace(r.URL.Query().Get("uuid"))
	if uuid == "" {
		sendJSONError(w, "Missing uuid", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	period := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	usage, err := h.state.Store.GetTrafficUsage(uuid, period)
	if err != nil {
		sendJSONError(w, "Failed to read usage", http.StatusInternalServerError)
		return
	}
	// The per-(region, kind) breakdown, each cell carrying the limit that
	// applies to it. Sent WITH the totals rather than behind a second endpoint:
	// the store asks this question hourly for every tenant, and a ceiling
	// assembled from two calls can be assembled from two different moments.
	//
	// Cells with no usage are omitted. They cannot be over a ceiling, and the
	// question "may this tenant buy more here" is asked at checkout, not here.
	//
	// A cell whose limit resolves to nil has no ceiling and is reported that
	// way. nil is not zero: zero would stop a tenant the operator never
	// configured a limit for.
	cells, err := h.state.Store.GetTrafficUsageRegions(uuid, period)
	if err != nil {
		sendJSONError(w, "Failed to read usage", http.StatusInternalServerError)
		return
	}
	// Non-regional kinds are folded into ONE cell BEFORE limits are resolved.
	//
	// File transfers have a single global allowance, so leaving them per region
	// would hand the store several cells that all resolve to the same row - and
	// each would then be judged against the FULL allowance, letting a tenant have
	// that allowance once per region it happened to move bytes in. Folding here
	// rather than in the store keeps the wire honest: one pool, one cell.
	type aggKey struct{ region, kind string }
	agg := map[aggKey]int64{}
	order := make([]aggKey, 0, len(cells))
	for _, c := range cells {
		k := aggKey{services.TrafficLimitRegion(c.Region, c.Kind), c.Kind}
		if _, seen := agg[k]; !seen {
			order = append(order, k)
		}
		agg[k] += c.Bytes
	}
	regions := make([]map[string]interface{}, 0, len(order))
	for _, k := range order {
		lim, err := services.ResolveTrafficLimit(h.state.Store, uuid, k.region, k.kind)
		if err != nil {
			sendJSONError(w, "Failed to resolve traffic limits", http.StatusInternalServerError)
			return
		}
		regions = append(regions, map[string]interface{}{
			"region":        k.region,
			"kind":          k.kind,
			"bytes":         agg[k],
			"includedGb":    lim.IncludedGB,    // per UNIT held; null = no limit
			"maxPurchaseGb": lim.MaxPurchaseGB, // per UNIT held; null = no limit, 0 = not for sale
			"decidedBy":     lim.Scope,         // "" = nothing configured anywhere
		})
	}

	// The backup allowance, resolved HERE. The store owns the money and the
	// consent; Core owns what a purchase includes and what may be booked on top,
	// and hands both over rather than letting the store keep a second copy of a
	// number an operator edits on a Core screen. That second copy is exactly how
	// the pricing page came to promise a traffic allowance nothing enforced.
	billing, _ := h.state.Store.GetUserBilling(uuid)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":          true,
		"period":           period.Format("2006-01-02"),
		"backupIncludedGb": services.R2IncludedGB(h.state.Store, billing),
		"backupBookableGb": services.R2BookableGB(h.state.Store, billing),
		"edgeBytes":        usage.EdgeBytes,
		"relayBytes":       usage.RelayBytes,
		"backupBytes":      usage.BackupBytes,
		// What they keep on a bucket of their OWN. Reported separately and never
		// added to backupBytes: it is not billed and not capped, but leaving it
		// off the response entirely would make a tenant storing 400 GB read as
		// storing nothing on a screen that says "backup storage".
		"backupBytesOwnStorage": backupBytesOnOwnStorage(h.state.Store, uuid),
		"regions":               regions,
	})
}

// backupBytesOnOwnStorage is a read that must never fail the usage response: it
// is informational, and the numbers beside it decide whether a tenant is billed.
// A failure reports zero and is not distinguished from "none", which is the one
// place that conflation is harmless - nothing acts on it.
func backupBytesOnOwnStorage(st store.Store, uuid string) int64 {
	n, err := st.BackupBytesByOwnerOnOwnStorage(uuid)
	if err != nil {
		return 0
	}
	return n
}

// userRouteScope is the per-tenant key in gateway_route_limits. Kept in one place
// because effectiveRouteLimit resolves "user:<id>" -> "user_default" -> "global"
// by string, so a typo here does not fail, it silently falls through to the
// platform default and hands out the wrong number of addresses.
func userRouteScope(userID string) string { return "user:" + userID }

// parseEntitlement decodes one purchased-entitlement field into the (value, set)
// pair SetUserPurchasedEntitlement wants. It has to distinguish three inbound
// states that Go's usual *int64 collapses into two: the field being absent (do
// not touch this column), an explicit null (clear the override), and a number.
//
// A non-positive count is treated as "clear", not as the literal value. A stored
// number is a PURCHASE, and a purchase wins over a live manual grant (see
// services.grantedCap): writing a store's "0 nodes" through would read as having
// bought zero and override a grant an admin gave separately. Cleared, the column
// says nothing, and the entitlement gate - not the cap - refuses what is not held.
func parseEntitlement(raw json.RawMessage) (*int64, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var v *int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, err
	}
	if v == nil || *v <= 0 {
		return nil, true, nil
	}
	return v, true, nil
}

// parseRoutePool decodes the purchased ADDRESS POOL, which cannot go through
// parseEntitlement even though it looks identical on the wire.
//
// The two need different answers for zero. parseEntitlement converts any
// non-positive number into "clear the override", because a stored count in
// user_billing is a purchase and would override a manual grant.
//
// max_routes lives in gateway_route_limits, where the user scope already has a
// perfectly good representation for zero: GetUserRouteLimit reports mode
// "disabled" for it and effectiveRouteLimit answers "Route creation is disabled
// for your account". A store that grants zero addresses is saying exactly that.
//
// Running it through the other table's convention threw that meaning away: the
// row was DELETED, the resolver fell through to user_default and then to global,
// and with neither set that is unlimited. The one number meaning "no addresses"
// produced no limit at all - the same trap as a zero node count, inverted.
//
// So: absent leaves the column alone, an explicit null clears the override (fall
// back to the platform default), and a number is written literally. A negative is
// clamped to 0 rather than cleared, because it is nonsense from the store and
// "none" is the safe reading of it - RoutePool already clamps the same way.
func parseRoutePool(raw json.RawMessage) (*int64, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var v *int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, err
	}
	if v == nil {
		return nil, true, nil
	}
	n := *v
	if n < 0 {
		n = 0
	}
	return &n, true, nil
}

// Provision POST /api/store/provision — store-key. The store (source of truth
// for payment) drives the Core billing lifecycle for a linked tenant:
//
//	action "activate" -> billing active (+ optional plan assignment, + the
//	                     node/route entitlement the tenant actually bought)
//	action "past_due" -> dunning grace window starts
//	action "suspend"  -> mark suspended now; servers stop + route-only links
//	                     are torn down after the grace window elapses (no data
//	                     deleted). Deliberately graced, not immediate: a buggy
//	                     webhook must not instant-kill a paying tenant. Compare
//	                     the admin-manual path (handlers/billing.go), which
//	                     uses the immediate SuspendNow instead.
//
// This is how a successful Stripe checkout / failed payment / canceled
// subscription reaches Core. It NEVER deletes user data.
func (h *StoreHandler) Provision(w http.ResponseWriter, r *http.Request) {
	if !h.requireStoreKey(w, r) {
		return
	}
	if h.state.Billing == nil {
		sendJSONError(w, "Billing not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		UUID   string `json:"uuid"`
		Action string `json:"action"`
		// planId is deliberately not read any more. Plan tiers are gone; nothing
		// resolves a plan, so assigning one wrote a column no reader consults
		// while answering the store with success. A field that is silently
		// ignored is better than one that reports work it did not do - and an
		// older store that still sends it now gets the entitlement applied from
		// maxNodes/maxLinks below, which is what it always actually needed.
		// MaxNodes/MaxLinks carry the PURCHASED entitlement. The store sells a node
		// COUNT (quantity x per-node price), not one of Core's plans, so the number
		// the customer paid for arrives here rather than as a planId. Omitted =
		// "the store did not say", which leaves the column alone; explicit null =
		// clear the override and fall back to the plan.
		MaxNodes json.RawMessage `json:"maxNodes,omitempty"`
		MaxLinks json.RawMessage `json:"maxLinks,omitempty"`
		// MaxRoutes is the tenant's protected-ADDRESS pool, which the store
		// derives from every product they hold. It lives in gateway_route_limits
		// (scope "user:<uuid>"), not in user_billing: routes were already capped
		// there by hand, and two caps for one thing is how they drift apart.
		MaxRoutes json.RawMessage `json:"maxRoutes,omitempty"`
		// TrafficCeilingGB is where the tenant's free traffic ends (decimal GB,
		// 10^9) and TrafficBillingEnabled whether they have agreed to be charged
		// past it. Core does not enforce either - the store owns the money and
		// does the stopping. They are stored so the PANEL can warn the tenant
		// while they can still act on it, which is the whole point of a ceiling
		// that stops things rather than billing them.
		TrafficCeilingGB      *int64 `json:"trafficCeilingGb,omitempty"`
		TrafficBillingEnabled *bool  `json:"trafficBillingEnabled,omitempty"`
		// BackupBillingEnabled is the SEPARATE consent for backup storage beyond
		// what the purchase includes. Its own flag: agreeing to pay for player
		// traffic is not agreeing to pay for stored backups.
		BackupBillingEnabled *bool `json:"backupBillingEnabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UUID == "" {
		sendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}

	user, err := h.state.Store.GetUserByID(req.UUID)
	if err != nil || user == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	switch req.Action {
	case "activate":
		if err := h.state.Billing.Reactivate(req.UUID); err != nil {
			sendJSONError(w, "Failed to activate billing", http.StatusInternalServerError)
			return
		}
		nodes, setNodes, nerr := parseEntitlement(req.MaxNodes)
		links, setLinks, lerr := parseEntitlement(req.MaxLinks)
		if nerr != nil || lerr != nil {
			sendJSONError(w, "Invalid entitlement", http.StatusBadRequest)
			return
		}
		if setNodes || setLinks {
			if err := h.state.Store.SetUserPurchasedEntitlement(req.UUID, nodes, setNodes, links, setLinks); err != nil {
				sendJSONError(w, "Activated but failed to apply entitlement", http.StatusInternalServerError)
				return
			}
			// A purchase RETIRES the manual grant for what was bought. The grant
			// is how somebody is given the product before they pay for it, so the
			// moment they do, it has done its job - and leaving it behind means
			// the account reads as "plan and grant" forever, shows a deadline
			// that decides nothing, and quietly stays entitled for the rest of
			// the grant if the subscription is later cancelled.
			//
			// Only a POSITIVE purchase retires it. The store also pushes zero,
			// which is a cancellation, and clearing the grant on that would take
			// away the thing an admin had granted separately.
			h.retireGrantsCoveredByPurchase(req.UUID, nodes, setNodes, links, setLinks)
		}
		// The consent flags, each on its own. This used to require the ceiling to
		// be present alongside the traffic flag - "both or neither", which was
		// right while the store computed the ceiling and became silently wrong
		// the day it stopped sending one: the condition never fired again, so a
		// tenant who switched metered billing ON stayed recorded as having
		// refused it, and the guard kept them stopped instead of billed.
		if req.TrafficBillingEnabled != nil || req.BackupBillingEnabled != nil {
			if err := h.state.Store.SetUserBillingConsent(req.UUID, req.TrafficBillingEnabled, req.BackupBillingEnabled); err != nil {
				sendJSONError(w, "Activated but failed to record the billing consent", http.StatusInternalServerError)
				return
			}
		}
		routes, setRoutes, rerr := parseRoutePool(req.MaxRoutes)
		if rerr != nil {
			sendJSONError(w, "Invalid entitlement", http.StatusBadRequest)
			return
		}
		if setRoutes {
			// Only an explicit NULL means "fall back to the user_default/global
			// scope", which is what deleting the per-user row does. A zero is a
			// number the store meant: this tenant gets no addresses on our
			// domains, which is what a 0 in this table already says.
			if routes == nil {
				if err := h.state.Store.DeleteGatewayRouteLimit(userRouteScope(req.UUID)); err != nil {
					sendJSONError(w, "Activated but failed to clear the route pool", http.StatusInternalServerError)
					return
				}
			} else {
				n := int(*routes)
				if err := h.state.Store.SetGatewayRouteLimit(userRouteScope(req.UUID), &n); err != nil {
					sendJSONError(w, "Activated but failed to apply the route pool", http.StatusInternalServerError)
					return
				}
			}
		}
	case "past_due":
		// Dunning never lifts a suspension. A failed payment retry reaching a
		// tenant already cut off used to put them back into grace, with
		// everything running again, on every retry Stripe made.
		if b, err := h.state.Store.GetUserBilling(req.UUID); err == nil && b != nil && b.Status == "suspended" {
			break
		}
		if err := h.state.Billing.EnterPastDue(req.UUID); err != nil {
			sendJSONError(w, "Failed to set past_due", http.StatusInternalServerError)
			return
		}
	case "suspend":
		if err := h.state.Billing.Suspend(r.Context(), req.UUID); err != nil {
			sendJSONError(w, "Failed to suspend", http.StatusInternalServerError)
			return
		}
	default:
		sendJSONError(w, "Unknown action", http.StatusBadRequest)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "action": req.Action})
}

// Status GET /api/store/status — authed panel user. Reads the link status from
// dylaris.com on demand (single-owned link: the store holds the join, Core does
// not). Drives the account-settings "Connect Store" / "Linked" UI.
func (h *StoreHandler) Status(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "enabled": false, "linked": false})
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	linked, email := h.fetchLinkStatus(r.Context(), userID)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"enabled":  true,
		"linked":   linked,
		"email":    email,
		"storeUrl": h.state.StoreURL,
	})
}

// fetchLinkStatus asks dylaris.com whether this Core UUID is linked. Best-effort:
// on any store-side error it reports "not linked" rather than failing the panel.
func (h *StoreHandler) fetchLinkStatus(ctx context.Context, uuid string) (bool, string) {
	linked, email, err := h.probeLinkStatus(ctx, uuid)
	if err != nil {
		// Still fail soft for the customer - a store outage must not break the
		// panel - but say so somewhere. Every one of these used to return a bare
		// false, so a wrong STORE_SHARED_KEY, an unreachable storefront and a
		// genuinely unlinked account produced the identical "Connect Store"
		// button. The one state that needs an operator was indistinguishable
		// from the two that do not.
		log.Printf("store: link-status lookup failed for %s: %v", uuid, err)
		return false, ""
	}
	return linked, email
}

// probeLinkStatus is fetchLinkStatus with the error kept, so the health check can
// report WHAT is wrong with the storefront integration instead of the panel
// silently rendering "not linked".
func (h *StoreHandler) probeLinkStatus(ctx context.Context, uuid string) (bool, string, error) {
	return h.state.probeStoreLink(ctx, uuid)
}

// probeStoreLink lives on AppState rather than on StoreHandler because the
// entitlement gate needs the same answer and holds no handler. It reads only
// StoreURL and StoreSharedKey, both of which are state.
func (s *AppState) probeStoreLink(ctx context.Context, uuid string) (bool, string, error) {
	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/link-status?uuid=" + url.QueryEscape(uuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("X-Store-Key", s.StoreSharedKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// The single most likely misconfiguration, and the one that looks most
		// like normal operation from the panel.
		return false, "", fmt.Errorf("the storefront refused our key (HTTP %d) - STORE_SHARED_KEY does not match the value configured on %s", resp.StatusCode, s.StoreURL)
	}
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("the storefront answered HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var parsed struct {
		Linked bool   `json:"linked"`
		Email  string `json:"email"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, "", fmt.Errorf("the storefront answered something that is not the expected JSON: %w", err)
	}
	return parsed.Linked, parsed.Email, nil
}

// BackupDefaults GET /api/store/backup-defaults - store-key. What ONE purchased
// unit brings in backup storage, and how much more may be booked on top.
//
// It exists so the public pricing page can print a real number without keeping
// a copy of it. The allowance is edited on a Core settings screen; a second copy
// in the store would be a second thing to change, and the one that gets
// forgotten is the one customers read. That is not hypothetical here - the
// pricing page promised a traffic allowance nothing enforced for exactly that
// reason, from exactly that shape.
//
// Per UNIT, not per tenant: this endpoint has no tenant. The store multiplies by
// what a plan sells.
func (h *StoreHandler) BackupDefaults(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	// A nil billing row means "no purchase", which is what per-unit asks for:
	// the setting itself, unmultiplied.
	one := &store.UserBilling{MaxNodes: ptrInt64(1)}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"includedGbPerUnit": services.R2IncludedGB(h.state.Store, one),
		"bookableGbPerUnit": services.R2BookableGB(h.state.Store, one),
	})
}

func ptrInt64(n int64) *int64 { return &n }

// retireGrantsCoveredByPurchase ends the manual grant for each kind the store
// has just sold, so the account is on its subscription from that moment on.
//
// Best-effort and logged rather than fatal: the purchase itself has already
// been recorded, and failing the provision call after that would have the store
// retry a purchase Core has already applied. A grant left behind is untidy;
// a double-applied purchase is not.
func (h *StoreHandler) retireGrantsCoveredByPurchase(userID string, nodes *int64, setNodes bool, links *int64, setLinks bool) {
	bought := func(v *int64, set bool) bool { return set && v != nil && *v > 0 }
	for _, c := range []struct {
		kind   string
		bought bool
	}{
		{services.EntitlementByon, bought(nodes, setNodes)},
		{services.EntitlementRouteOnly, bought(links, setLinks)},
	} {
		if !c.bought {
			continue
		}
		if err := h.state.Store.SetUserManualEntitlementKind(userID, c.kind, nil, ""); err != nil {
			log.Printf("provision: purchased %s for %s but could not retire the manual grant: %v", c.kind, userID, err)
		}
	}
}
