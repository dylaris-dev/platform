package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// Route-only ("via Link") routes: a protected address pointed at a server
// the customer runs on their OWN machine, reached through their own outbound Link
// tunnel — no managed node, no exposed origin. The customer runs one link
// container that talks to Core over HTTPS; the edge opens a stream on their Link
// and the Link dials the LOCAL target. Splice + rolling updates work exactly as for a
// managed server, because it uses the same tunnel path. The Link's own
// allow-list (LINK_ALLOWED_TARGETS) is the authority on what it will dial.

// validateLocalTarget sanity-checks a route-only target. The target is dialed by
// the customer's Link on its LOCAL network, so LAN / loopback / private addresses
// are EXPECTED and allowed here — the Link, not Core, decides what it will dial.
// We only reject empty or malformed input.
func validateLocalTarget(host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return fmt.Errorf("target host is required")
	}
	// The Link treats exactly one address as "this machine": 127.0.0.1, which it
	// allows without a list and rewrites to the host on Docker Desktop. ::1 gets
	// neither, so the route would be refused by the Link with nothing in the
	// panel saying why.
	if ip := net.ParseIP(host); ip != nil {
		// Any other spelling of loopback, ::1 or ::ffff:127.0.0.1 alike.
		if ip.IsLoopback() && strings.Contains(host, ":") {
			return fmt.Errorf("use 127.0.0.1 for this machine")
		}
		return nil // any other IP literal, including LAN / loopback
	}
	// Hostname (e.g. localhost, my-pc.local): basic format check, no wildcard.
	if strings.HasPrefix(host, "*.") || !domainRegex.MatchString(host) {
		return fmt.Errorf("invalid target host")
	}
	return nil
}

// effectiveRouteLimit resolves the cap on addresses ON OUR DOMAINS for one
// tenant. Returns nil for "no cap".
//
// Three scopes, most specific first: "user:<id>", then "user_default", then
// "global". A scope with NO ROW says nothing and the next one is asked. A scope
// WITH a row answers, and its answer may be NULL - "I am set, and I set no cap" -
// which is a decision and therefore stops the walk.
//
// That distinction is the whole point. This used to be one int where 0 meant
// "disabled" at the user scope and "ignore me" at the other two, so the same
// stored number meant two different things depending on which row it sat in, and
// a computed zero from the store was silently rewritten into "no override" and
// fell through to a scope nobody had set - which is unlimited. Now zero means
// none wherever it is, and absence is spelled by the row not existing.
func (h *GatewayHandler) effectiveRouteLimit(userID string) *int64 {
	for _, scope := range []string{"user:" + userID, "user_default", "global"} {
		l, err := h.state.Store.GetGatewayRouteLimit(scope)
		if err != nil || l == nil {
			continue // no row here: ask the next scope
		}
		if l.MaxRoutes == nil {
			return nil // set, and explicitly no cap
		}
		n := int64(*l.MaxRoutes)
		return &n
	}
	return nil
}

// countOwnerRoutes counts the tenant's addresses ON OUR DOMAINS. Routes on a
// domain the customer brought themselves are not counted and not capped: we hand
// out subdomains from a namespace that can run out, and a CNAME from their own
// domain costs us nothing to allow. See services.DomainIsOurs.
func (h *GatewayHandler) countOwnerRoutes(userID string) int {
	n, _ := h.ownerRouteStats(userID, "")
	return n
}

// ownerRouteStats counts the tenant's addresses on our domains and, in the same
// pass, reports whether `domain` is one they ALREADY hold.
//
// The second answer is what separates creating an address from editing one. A
// tenant changes a route's target by posting the same domain again - that is the
// only overwrite CreateRouteViaLink permits - and the allowance check counts
// what they already hold. Without this, a tenant sitting exactly on their cap
// could not change the port of a route THEY OWN: the check would refuse them
// over the very route it was counting, and the message would tell them to buy
// more addresses to keep the number of addresses the same.
func (h *GatewayHandler) ownerRouteStats(userID, domain string) (count int, ownsDomain bool) {
	bases := h.ourBaseDomains()
	domain = strings.ToLower(strings.TrimSpace(domain))
	for _, rt := range services.GetRoutesFromRedis(h.ctx(), h.state.Redis) {
		if rt.OwnerID != userID {
			continue
		}
		// CoreOwned as well as owned BY THEM: a route-only entry is the only
		// kind CreateRouteViaLink will overwrite, so it is the only kind an
		// edit can be. A managed server's route on the same domain would
		// otherwise wave the allowance check through for a request the gateway
		// then refuses anyway.
		if domain != "" && rt.CoreOwned && strings.EqualFold(rt.Domain, domain) {
			ownsDomain = true
		}
		if services.DomainIsOurs(rt.Domain, bases) {
			count++
		}
	}
	return count, ownsDomain
}

// ourBaseDomains is the hoster list the route cap is measured against.
func (h *GatewayHandler) ourBaseDomains() []string {
	raw, _ := h.state.Store.GetSetting(services.HosterDomainsSettingKey)
	return services.HosterBaseDomains(raw)
}

// resolveOwnedLinkToken confirms the caller owns the link kit identified by
// linkID and returns its derived Link tunnel token. Ownership is enforced by
// only ever looking at the caller's own kits, so a tenant can never point a
// route at another tenant's Link.
func (h *GatewayHandler) resolveOwnedLinkToken(userID, linkID string) (string, error) {
	linkID = strings.TrimSpace(linkID)
	if linkID == "" {
		return "", fmt.Errorf("link is required")
	}
	kits, err := h.state.Store.ListWarpAPIKeysByOwner(userID)
	if err != nil {
		return "", fmt.Errorf("failed to load your links")
	}
	for _, k := range kits {
		if k.NodeID == linkID {
			return h.state.Gateway.LinkToken(linkID), nil
		}
	}
	return "", fmt.Errorf("unknown link")
}

// CreateLinkRoute POST /api/gateway/link-routes — authed, gateway-gated. 409
// means the domain is already routed to someone else.
func (h *GatewayHandler) CreateLinkRoute(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)
	if !routeOnlyActive(h.state, r) {
		sendJSONError(w, "Route-only is not enabled", http.StatusForbidden)
		return
	}
	// The same gate minting a kit passes. Only the panel checked it before, so a
	// suspended tenant could still claim addresses through the API.
	if !h.state.requireEntitlement(r, w, userID, services.EntitlementRouteOnly) {
		return
	}

	var req struct {
		LinkID       string `json:"linkId"`
		Domain       string `json:"domain"`
		Subdomain    string `json:"subdomain"`
		HosterDomain string `json:"hosterDomain"`
		CustomDomain string `json:"customDomain"`
		TargetHost   string `json:"targetHost"`
		TargetPort   int    `json:"targetPort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.TargetPort == 0 {
		req.TargetPort = 25565
	}
	if req.TargetPort < 1 || req.TargetPort > 65535 {
		http.Error(w, "Invalid target port", http.StatusBadRequest)
		return
	}
	if err := validateLocalTarget(req.TargetHost); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Resolve (and authorize) the tenant's Link before doing anything else.
	linkToken, err := h.resolveOwnedLinkToken(userID, req.LinkID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Route creation switched off for this account is an operator decision about
	// the tenant, not about a domain, so it is answered before anything else -
	// and before any input validation can turn it into a confusing 400.
	limit := h.effectiveRouteLimit(userID)
	if limit != nil && *limit == 0 {
		http.Error(w, "Route creation is disabled for your account", http.StatusForbidden)
		return
	}

	finalDomain, err := h.resolveRouteDomain(&struct {
		Domain       string `json:"domain"`
		Subdomain    string `json:"subdomain"`
		HosterDomain string `json:"hosterDomain"`
		CustomDomain string `json:"customDomain"`
		TargetPort   int    `json:"targetPort"`
	}{
		Domain: req.Domain, Subdomain: req.Subdomain, HosterDomain: req.HosterDomain,
		CustomDomain: req.CustomDomain, TargetPort: req.TargetPort,
	}, IsAdmin(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The allowance itself can only be spent once we know whose domain this is: a
	// route the tenant points at us from their OWN domain costs the allowance
	// nothing, so refusing it on a full one would deny something we do not ration.
	//
	// An edit spends nothing either - see ownerRouteStats.
	held, isEdit := h.ownerRouteStats(userID, finalDomain)
	if !isEdit && services.DomainIsOurs(finalDomain, h.ourBaseDomains()) && services.AtOrOver(limit, int64(held)) {
		http.Error(w, fmt.Sprintf("You have used all %d addresses on our domains. Point your own domain at us instead - that is unlimited.", *limit), http.StatusForbidden)
		return
	}

	// Ownership proof for a domain the tenant brought themselves. Admins skip it.
	isCustomDomain := strings.TrimSpace(req.CustomDomain) != ""
	if gErr := h.customDomainGate(r, userID, finalDomain, isCustomDomain); gErr != nil {
		http.Error(w, gErr.Error(), http.StatusForbidden)
		return
	}
	if strings.HasPrefix(finalDomain, "*.") {
		http.Error(w, "Wildcard domains are not allowed for route-only", http.StatusBadRequest)
		return
	}

	host := strings.TrimSpace(strings.ToLower(req.TargetHost))
	if host == "localhost" {
		// Same reason as ::1 in validateLocalTarget, but unambiguous enough to
		// fix rather than refuse.
		host = "127.0.0.1"
	}
	if err := h.state.Gateway.CreateRouteViaLink(userID, finalDomain, linkToken, host, req.TargetPort); err != nil {
		msg := err.Error()
		if errors.Is(err, services.ErrRouteDomainTaken) {
			// Same wording the availability hint uses, and deliberately silent
			// about who holds it.
			http.Error(w, fmt.Sprintf("%s is already in use", finalDomain), http.StatusConflict)
		} else if strings.Contains(msg, "disabled") {
			http.Error(w, msg, http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("Failed to create route: %s", msg), http.StatusInternalServerError)
		}
		return
	}
	// After the route exists, not before - see armCustomDomainClaim.
	ownershipNotice := h.armCustomDomainClaim(r, userID, finalDomain, isCustomDomain)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Route created",
		"domain": finalDomain, "ownershipNotice": ownershipNotice})
}

// ownsCoreRoute reports whether this tenant may remove that route.
//
// The stored row is asked first and the live Redis entry second, because either
// one proves ownership and they can disagree: a route whose Redis entry is gone
// is exactly the route a tenant most wants to delete, and asking only the cache
// meant the delete answered "Route not found" for something the panel had just
// listed. The reverse gap is the create path's rollback window.
func (h *GatewayHandler) ownsCoreRoute(userID, domain string) bool {
	if rows, err := h.state.Store.ListCoreLinkRoutes(); err == nil {
		for _, rt := range rows {
			if rt.Domain == domain && rt.OwnerID == userID {
				return true
			}
		}
	}
	for _, rt := range services.GetRoutesFromRedis(h.ctx(), h.state.Redis) {
		if rt.Domain == domain && rt.CoreOwned && rt.OwnerID == userID {
			return true
		}
	}
	return false
}

// ListLinkRoutes GET /api/gateway/link-routes — the caller's route-only entries.
func (h *GatewayHandler) ListLinkRoutes(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)

	// Which LINK each route runs through, resolved here because only this side
	// can. A route stores the link's derived tunnel TOKEN, which is a secret and
	// must not be listed; the panel knows kits by their link id. Without the
	// translation the edit form cannot show which link a route uses, and a save
	// would quietly move it to whichever link happened to be selected.
	linkIDByToken := map[string]string{}
	if kits, err := h.state.Store.ListWarpAPIKeysByOwner(userID); err == nil {
		for _, k := range kits {
			if k.NodeID != "" {
				linkIDByToken[h.state.Gateway.LinkToken(k.NodeID)] = k.NodeID
			}
		}
	}

	// services.PublicRoute, so the tunnel token stays out of the response for the
	// same reason as everywhere else: it is the link's credential at the edge,
	// not a description of the route. LinkID is what the UI needs instead.
	type linkRoute struct {
		services.PublicRoute
		LinkID string `json:"link_id,omitempty"`
	}
	out := make([]linkRoute, 0)
	for _, rt := range services.GetRoutesFromRedis(h.ctx(), h.state.Redis) {
		if rt.CoreOwned && rt.OwnerID == userID {
			out = append(out, linkRoute{PublicRoute: rt.Public(), LinkID: linkIDByToken[rt.TunnelID]})
		}
	}
	json.NewEncoder(w).Encode(out)
}

// DeleteLinkRoute DELETE /api/gateway/link-routes/{domain} — owner-scoped.
func (h *GatewayHandler) DeleteLinkRoute(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)
	domain := mux.Vars(r)["domain"]
	if domain == "" {
		http.Error(w, "domain required", http.StatusBadRequest)
		return
	}
	if !isAdmin && !h.ownsCoreRoute(userID, domain) {
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}
	if err := h.state.Gateway.DeleteCoreOwnedRoute(domain); err != nil {
		http.Error(w, "Failed to delete route", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Route deleted"})
}
