package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"dylaris-core/services"

	"github.com/gorilla/mux"
)

type GatewayHandler struct {
	state *AppState
}

func NewGatewayHandler(state *AppState) *GatewayHandler {
	return &GatewayHandler{state: state}
}

var domainRegex = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// Per-hoster subdomain validation regexes — kept narrow on purpose so the
// admin's choice in settings actually constrains what users can register.
var (
	subRegexLetters      = regexp.MustCompile(`^[a-z]+$`)
	subRegexAlphanumeric = regexp.MustCompile(`^[a-z0-9]+$`)
	subRegexDNS          = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// validateSubdomain matches a user-entered subdomain against the validation
// mode the admin picked for that hoster domain.
func validateSubdomain(sub, mode string) bool {
	if sub == "" || len(sub) > 63 {
		return false
	}
	switch mode {
	case "letters":
		return subRegexLetters.MatchString(sub)
	case "alphanumeric":
		return subRegexAlphanumeric.MatchString(sub)
	case "dns":
		return subRegexDNS.MatchString(sub)
	default:
		return false
	}
}

func (h *GatewayHandler) ctx() context.Context {
	return context.Background()
}

// ==========================================
// ADMIN: Links
// ==========================================

// GetLinks GET /api/gateway/links - every link registered in Redis, each with
// its online flag.
func (h *GatewayHandler) GetLinks(w http.ResponseWriter, r *http.Request) {
	// A link IS its token here - the Redis key is link:<token> - so identifying
	// one without handing over the credential means naming it by a digest
	// instead. The whole token would let its holder claim that link's tunnel at
	// the edge and take a share of its players; the digest identifies the same
	// link across polls, which is all a listing needs. Nothing has ever read the
	// full value on the client side.
	links := services.GetLinksFromRedis(h.ctx(), h.state.Redis)
	out := make([]map[string]interface{}, 0, len(links))
	for _, l := range links {
		out = append(out, map[string]interface{}{
			"id":     services.LinkFingerprint(l.Token),
			"online": l.Online,
		})
	}
	json.NewEncoder(w).Encode(out)
}

// ==========================================
// ADMIN: Edges
// ==========================================

// GetEdges GET /api/gateway/edges - every edge currently registered in Redis.
// Edges are auto-discovered from their own heartbeats, never configured in
// Core.
func (h *GatewayHandler) GetEdges(w http.ResponseWriter, r *http.Request) {
	edges := services.GetEdgesFromRedis(h.ctx(), h.state.Redis)
	if edges == nil {
		edges = []services.GatewayEdgeInfo{}
	}
	json.NewEncoder(w).Encode(edges)
}

// ==========================================
// ADMIN: Routes (all routes overview)
// ==========================================

// GetAllRoutes GET /api/gateway/routes - every gateway route across the whole
// fleet, read from Redis.
func (h *GatewayHandler) GetAllRoutes(w http.ResponseWriter, r *http.Request) {
	// PublicRoutes, not the stored shape: every route carries the tunnel token
	// its link authenticates with, and this listing spans the whole fleet.
	json.NewEncoder(w).Encode(services.PublicRoutes(services.GetRoutesFromRedis(h.ctx(), h.state.Redis)))
}

// AdminDeleteRoute DELETE /api/gateway/routes/{domain} - removes any route by
// domain, without the per-server ownership check its /api/servers twin
// applies.
func (h *GatewayHandler) AdminDeleteRoute(w http.ResponseWriter, r *http.Request) {
	domain := mux.Vars(r)["domain"]
	if domain == "" {
		http.Error(w, "domain required", http.StatusBadRequest)
		return
	}
	// JSON on the failure path too. http.Error writes text/plain, and the panel
	// reads res.error out of a JSON body - so a refused delete arrived as an
	// undefined field and was reported with a generic message that named
	// nothing.
	if err := h.state.Gateway.DeleteRoute(domain); err != nil {
		log.Printf("AdminDeleteRoute: %s: %v", domain, err)
		sendJSONError(w, "Failed to delete route", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "Route deleted"})
}

// BulkDeleteRoutesBySuffix deletes every route whose domain equals OR ends
// with `.<suffix>`. Independent of the hoster-domain list in settings — it
// operates purely on what's currently in the routes table / Redis cache.
// POST /api/gateway/routes/bulk-delete  body: {"suffix": "mc.example.com"}
func (h *GatewayHandler) BulkDeleteRoutesBySuffix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix string `json:"suffix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	suffix := strings.ToLower(strings.TrimSpace(req.Suffix))
	if suffix == "" {
		sendJSONError(w, "suffix required", http.StatusBadRequest)
		return
	}

	routes := services.GetRoutesFromRedis(h.ctx(), h.state.Redis)
	deleted := 0
	failed := 0
	for _, rt := range routes {
		d := strings.ToLower(rt.Domain)
		if d == suffix || strings.HasSuffix(d, "."+suffix) {
			if err := h.state.Gateway.DeleteRoute(rt.Domain); err != nil {
				failed++
			} else {
				deleted++
			}
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"deleted": deleted,
		"failed":  failed,
		"suffix":  suffix,
	})
}

// GetRouteSuffixes returns the unique apex (1-dot) and parent (2-dot) suffixes
// across every route currently registered. Powers the bulk-delete picker.
// GET /api/gateway/routes/suffixes
func (h *GatewayHandler) GetRouteSuffixes(w http.ResponseWriter, r *http.Request) {
	routes := services.GetRoutesFromRedis(h.ctx(), h.state.Redis)
	seen := map[string]int{} // suffix → match count
	for _, rt := range routes {
		d := strings.ToLower(strings.TrimSpace(rt.Domain))
		if strings.HasPrefix(d, "*.") {
			d = d[2:]
		}
		labels := strings.Split(d, ".")
		// Build apex (last 2 labels) and parent (last 3 labels if present).
		if len(labels) >= 2 {
			apex := strings.Join(labels[len(labels)-2:], ".")
			seen[apex]++
		}
		if len(labels) >= 3 {
			parent := strings.Join(labels[len(labels)-3:], ".")
			seen[parent]++
		}
	}
	type SuffixEntry struct {
		Suffix string `json:"suffix"`
		Count  int    `json:"count"`
		Depth  int    `json:"depth"` // 1 = apex (one dot), 2 = parent (two dots)
	}
	out := make([]SuffixEntry, 0, len(seen))
	for s, c := range seen {
		depth := strings.Count(s, ".")
		out = append(out, SuffixEntry{Suffix: s, Count: c, Depth: depth})
	}
	// Sort apex first, then by count desc, then alpha
	sort.Slice(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Suffix < out[j].Suffix
	})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"suffixes": out,
	})
}

// ==========================================
// ADMIN: Stats, Sync
// ==========================================

// GetLogs GET /api/gateway/logs - hub logs are not shipped to Redis, so this
// answers with the last 100 service error entries instead.
func (h *GatewayHandler) GetLogs(w http.ResponseWriter, r *http.Request) {
	// Hub logs are not in Redis; return service error logs instead
	errors := services.GetAllServiceErrorsFromRedis(h.state.Redis, 100)
	json.NewEncoder(w).Encode(errors)
}

// GetStats GET /api/gateway/stats - counts only: links and edges, how many of
// each are online, and the route total.
func (h *GatewayHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	links := services.GetLinksFromRedis(h.ctx(), h.state.Redis)
	edges := services.GetEdgesFromRedis(h.ctx(), h.state.Redis)
	routeCount := services.CountRoutesFromRedis(h.ctx(), h.state.Redis)

	onlineLinks := 0
	for _, l := range links {
		if l.Online {
			onlineLinks++
		}
	}
	onlineEdges := 0
	for _, e := range edges {
		if e.Status == "online" {
			onlineEdges++
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"links":       len(links),
		"linksOnline": onlineLinks,
		"edges":       len(edges),
		"edgesOnline": onlineEdges,
		"routes":      routeCount,
	})
}

// TriggerSync POST /api/gateway/sync - a no-op kept for the panel button. The
// hub syncs autonomously, so Core has nothing to trigger.
func (h *GatewayHandler) TriggerSync(w http.ResponseWriter, r *http.Request) {
	// Sync is managed by Hub autonomously; no-op from Core side
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Sync is managed by Hub"})
}

// ==========================================
// ADMIN: Errors
// ==========================================

// GetErrors GET /api/gateway/errors - the 50 most recent gateway service
// errors; ?service= narrows them to one component.
func (h *GatewayHandler) GetErrors(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != "" {
		errors := services.GetServiceErrorsFromRedis(h.state.Redis, service, 50)
		json.NewEncoder(w).Encode(errors)
	} else {
		errors := services.GetAllServiceErrorsFromRedis(h.state.Redis, 50)
		json.NewEncoder(w).Encode(errors)
	}
}

// CheckDomainAvailability tells the panel whether a candidate domain is
// already registered, so the route-create form can show a live "available
// / in use" hint while the user types. Resolves the same three input
// shapes as CreateServerRoute (subdomain+hosterDomain, customDomain, or
// raw domain) and answers only `{available}` — never leaks who owns a
// taken domain.
//
// GET /api/gateway/check-domain?domain=foo.example.com
// GET /api/gateway/check-domain?subdomain=foo&hosterDomain=mc.example.com
// GET /api/gateway/check-domain?customDomain=play.acme.tld
func (h *GatewayHandler) CheckDomainAvailability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := struct {
		Domain       string `json:"domain"`
		Subdomain    string `json:"subdomain"`
		HosterDomain string `json:"hosterDomain"`
		CustomDomain string `json:"customDomain"`
		TargetPort   int    `json:"targetPort"`
	}{
		Domain:       q.Get("domain"),
		Subdomain:    q.Get("subdomain"),
		HosterDomain: q.Get("hosterDomain"),
		CustomDomain: q.Get("customDomain"),
	}
	finalDomain, _, err := h.resolveRouteDomain(&req, IsAdmin(r))
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available": false,
			"reason":    err.Error(),
		})
		return
	}

	exists, _ := h.state.Redis.Exists(h.ctx(), "route:"+finalDomain).Result()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"available": exists == 0,
		"domain":    finalDomain,
	})
}

// ==========================================
// USER: Server Routes
// ==========================================

// GetServerRoutes GET /api/servers/{id}/routes - the gateway routes belonging
// to one server, filtered out of the full Redis set by server UUID.
func (h *GatewayHandler) GetServerRoutes(w http.ResponseWriter, r *http.Request) {
	serverID := mux.Vars(r)["id"]

	server, err := h.state.Store.GetServerByID(mustAtoi(serverID))
	if err != nil {
		http.Error(w, "Server not found", http.StatusNotFound)
		return
	}

	var routes []services.GatewayRoute
	for _, rt := range services.GetRoutesFromRedis(h.ctx(), h.state.Redis) {
		if rt.ServerUUID == server.UUID {
			routes = append(routes, rt)
		}
	}
	// A managed route's tunnel token is derived from the NODE, so it is the same
	// credential for every server that node hosts. This endpoint needs only
	// network.read on ONE server, which an ordinary tenant holds on their own.
	json.NewEncoder(w).Encode(services.PublicRoutes(routes))
}

// CreateServerRoute POST /api/servers/{id}/routes - queues a route for one
// server. Three input shapes are accepted in priority order: {subdomain,
// hosterDomain} picked from the admin's hoster list, {customDomain} for a
// domain the user owns via CNAME, and a raw {domain} for scripts. 201 means
// queued, not live; 409 means the domain is already routed to someone else.
func (h *GatewayHandler) CreateServerRoute(w http.ResponseWriter, r *http.Request) {
	serverID := mustAtoi(mux.Vars(r)["id"])
	userID := r.Context().Value("userID").(string)

	// Three input shapes, listed in priority order:
	//   1. {subdomain, hosterDomain}      — user picked from the admin's hoster list
	//   2. {customDomain}                 — user brings their own domain (CNAME path)
	//   3. {domain}                       — legacy / admin / scripts; raw FQDN
	var req struct {
		Domain       string `json:"domain"`
		Subdomain    string `json:"subdomain"`
		HosterDomain string `json:"hosterDomain"`
		CustomDomain string `json:"customDomain"`
		TargetPort   int    `json:"targetPort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.TargetPort == 0 {
		req.TargetPort = 25565
	}

	// isCustomDomain comes from the resolver, not from which FIELD the request
	// used: a tenant's raw FQDN is normalised into a brought domain, and reading
	// the field here would have left that one unproven.
	finalDomain, isCustomDomain, err := h.resolveRouteDomain(&req, IsAdmin(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Ownership proof for a domain the tenant brought themselves. Admins skip it.
	if gErr := h.customDomainGate(r, userID, finalDomain, isCustomDomain); gErr != nil {
		http.Error(w, gErr.Error(), http.StatusForbidden)
		return
	}

	// The gateway routes Minecraft TCP only (WS7): the HTTP/HTTPS edge ingress
	// was removed, so web (80/443) target ports and wildcard domains - which
	// only ever existed for that web data-plane - are no longer accepted. MC
	// routes target a specific hostname on an MC port.
	if req.TargetPort == 80 || req.TargetPort == 443 {
		http.Error(w, "Web routes (port 80/443) are no longer supported; the gateway routes Minecraft TCP only", http.StatusBadRequest)
		return
	}
	if strings.HasPrefix(finalDomain, "*.") {
		http.Error(w, "Wildcard domains are not supported", http.StatusBadRequest)
		return
	}

	// The same per-user route cap CreateLinkRoute applies, and for the same
	// reason: countOwnerRoutes counts EVERY route owned by this user, so the two
	// endpoints spend one shared allowance and only one of them was checking it.
	//
	// A tenant capped at N link routes could create unlimited managed-server
	// routes on the same account, and - the sharper half - the explicit
	// "disabled" mode an operator sets on /api/users/{id}/route-limit to stop an
	// abusive tenant was honoured on one door and ignored on the other.
	//
	// RedisGateway.CreateServerRoute says "limit counts skipped - Hub enforces
	// uniqueness in its DB", which is true and unrelated: uniqueness stops two
	// routes sharing a domain, it does not bound how many one account may hold.
	// The cap lives here because effectiveRouteLimit reads Core's settings.
	//
	// Admins are exempt. The cap is a tenant allowance, and an admin registering
	// the platform's own routes must not be blocked by user_default - the same
	// asymmetry resolveRouteDomain already applies to the reserved-name list.
	//
	// The cap counts only addresses on OUR domains, matching CreateLinkRoute and
	// the over-limit sweep. A tenant who points their own domain at us is not
	// spending anything we ration.
	if !IsAdmin(r) {
		limit := h.effectiveRouteLimit(userID)
		if limit != nil && *limit == 0 {
			http.Error(w, "Route creation is disabled for your account", http.StatusForbidden)
			return
		}
		if services.DomainIsOurs(finalDomain, h.ourBaseDomains()) && services.AtOrOver(limit, int64(h.countOwnerRoutes(userID))) {
			http.Error(w, fmt.Sprintf("You have used all %d addresses on our domains. Point your own domain at us instead - that is unlimited.", *limit), http.StatusForbidden)
			return
		}
	}

	if err := h.state.Gateway.CreateServerRoute(uint(serverID), userID, finalDomain, req.TargetPort); err != nil {
		errMsg := err.Error()
		if errors.Is(err, services.ErrRouteDomainTaken) {
			http.Error(w, fmt.Sprintf("%s is already in use", finalDomain), http.StatusConflict)
		} else if strings.Contains(errMsg, "not found") {
			http.Error(w, errMsg, http.StatusNotFound)
		} else if strings.Contains(errMsg, "disabled") {
			http.Error(w, errMsg, http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("Failed to create route: %s", errMsg), http.StatusInternalServerError)
		}
		return
	}

	// Armed only now that the route exists: every rejection above would
	// otherwise have left a live claim, and its deadline counts against the
	// customer whether or not they ever got a route.
	ownershipNotice := h.armCustomDomainClaim(r, userID, finalDomain, isCustomDomain)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Route creation queued",
		"domain":  finalDomain,
		// Empty unless a custom-domain grant was just armed. The customer has
		// four hours from here, and nothing else tells them.
		"ownershipNotice": ownershipNotice,
	})
}

// resolveRouteDomain inspects the three accepted input shapes and returns the
// final lowercase FQDN to register, plus whether it is a domain the TENANT
// brought themselves - which is what decides whether the ownership proof runs.
// It enforces the admin's hoster-domain configuration: subdomains must match
// the per-hoster validation mode, custom domains may only be used when the
// admin enabled them and may not collide with any hoster domain.
//
// isAdmin lifts the blocked-prefix check (the reserved names exist to keep
// confusable ones away from tenants; an admin is who they are reserved FOR)
// and keeps the raw-FQDN escape hatch open. A tenant has no escape hatch: see
// the raw path below for what that used to cost.
func (h *GatewayHandler) resolveRouteDomain(req *struct {
	Domain       string `json:"domain"`
	Subdomain    string `json:"subdomain"`
	HosterDomain string `json:"hosterDomain"`
	CustomDomain string `json:"customDomain"`
	TargetPort   int    `json:"targetPort"`
}, isAdmin bool) (string, bool, error) {
	hosters, customEnabled, _ := h.loadGatewayDomainConfig()
	blocked := h.loadBlockedRoutePrefixes()
	if isAdmin {
		blocked = nil
	}

	// 1) Hoster-picker path
	if req.Subdomain != "" || req.HosterDomain != "" {
		sub := strings.ToLower(strings.TrimSpace(req.Subdomain))
		host := strings.ToLower(strings.TrimSpace(req.HosterDomain))
		if sub == "" || host == "" {
			return "", false, fmt.Errorf("subdomain and hosterDomain must both be set")
		}
		var hd *HosterDomain
		for i := range hosters {
			if hosters[i].Domain == host {
				hd = &hosters[i]
				break
			}
		}
		if hd == nil {
			return "", false, fmt.Errorf("hoster domain not configured: %s", host)
		}
		return resolveHosterSubdomain(sub, *hd, blocked)
	}

	// 2) Custom-domain path
	if req.CustomDomain != "" {
		return resolveBroughtDomain(strings.ToLower(strings.TrimSpace(req.CustomDomain)), hosters, customEnabled, blocked)
	}

	// 3) Raw-FQDN path (admin tools, scripts, backwards compat).
	//
	// For an ADMIN it stays what it says it is: whatever you type is registered.
	//
	// For a tenant it is not a third kind of address, it is one of the two above
	// written differently - and taking it at face value meant every rule the two
	// above enforce could be skipped by moving the same string into this field.
	// Measured on production: an ordinary account registered a foreign domain
	// while custom domains were switched OFF platform-wide, with no ownership
	// proof, and a name on our own apex that the picker's format rules would have
	// refused - and neither counted against its address allowance, because the
	// allowance only counts the configured hoster domains. First registration
	// wins, so that is a squatting lever, not just an untidy input.
	//
	// So a tenant's raw domain is normalised into whichever path it really is and
	// answers to that path's rules, the ownership proof included (the isCustom
	// return is what arms it at the call site).
	if req.Domain != "" {
		dom := strings.ToLower(strings.TrimSpace(req.Domain))
		if !domainRegex.MatchString(dom) {
			return "", false, fmt.Errorf("invalid domain format")
		}
		if isAdmin {
			return dom, false, nil
		}
		// An operator who has configured NO address policy at all - no hoster
		// domain to pick from, custom domains off - has not said what a tenant may
		// claim, and there is nothing to ration either. That is the install the
		// panel's own picker calls "legacy" and falls back to, so closing this here
		// would leave those tenants with no way to create an address at all. The
		// reserved names still hold.
		if len(hosters) == 0 && !customEnabled {
			if labels := strings.Split(dom, "."); len(labels) > 0 && blocked[labels[0]] {
				return "", false, fmt.Errorf("leftmost label %q is reserved", labels[0])
			}
			return dom, false, nil
		}
		for _, hd := range hosters {
			if dom == hd.Domain || strings.HasSuffix(dom, "."+hd.Domain) {
				sub := strings.TrimSuffix(strings.TrimSuffix(dom, hd.Domain), ".")
				return resolveHosterSubdomain(sub, hd, blocked)
			}
		}
		return resolveBroughtDomain(dom, hosters, customEnabled, blocked)
	}

	return "", false, fmt.Errorf("no domain provided")
}

// resolveHosterSubdomain answers the subdomain-picker rules for one hoster
// domain. Shared by the picker path and by a tenant's raw FQDN that turns out
// to sit under a hoster domain, so the two cannot disagree about what a valid
// name is - which is exactly how the raw path came to accept names the picker
// refuses.
//
// An empty sub (the caller typed the hoster apex itself) fails validation, and
// that is the intended answer: the apex is not a tenant's to route.
func resolveHosterSubdomain(sub string, hd HosterDomain, blocked map[string]bool) (string, bool, error) {
	if !validateSubdomain(sub, hd.Validation) {
		return "", false, fmt.Errorf("subdomain does not match the allowed format for %s", hd.Domain)
	}
	if blocked[sub] {
		return "", false, fmt.Errorf("subdomain %q is reserved", sub)
	}
	return sub + "." + hd.Domain, false, nil
}

// resolveBroughtDomain answers the rules for a domain the TENANT owns rather
// than one of ours. It reports isCustom=true on success so the caller runs the
// ownership gate and arms the claim: a domain reaching us through any input
// shape has to prove itself the same way.
func resolveBroughtDomain(dom string, hosters []HosterDomain, customEnabled bool, blocked map[string]bool) (string, bool, error) {
	if !customEnabled {
		return "", false, fmt.Errorf("custom domains are not enabled")
	}
	if !domainRegex.MatchString(dom) || strings.HasPrefix(dom, "*.") {
		return "", false, fmt.Errorf("invalid custom domain format")
	}
	labels := strings.Split(dom, ".")
	// Apex (mc.de = 2 labels) up to apex + 2 subdomains (a.b.c.d = 4 labels)
	if len(labels) < 2 || len(labels) > 4 {
		return "", false, fmt.Errorf("custom domain may have at most two subdomain levels")
	}
	if blocked[labels[0]] {
		return "", false, fmt.Errorf("leftmost label %q is reserved", labels[0])
	}
	for _, h := range hosters {
		if dom == h.Domain || strings.HasSuffix(dom, "."+h.Domain) {
			return "", false, fmt.Errorf("custom domain may not be a subdomain of a hoster domain (%s) — use the subdomain picker instead", h.Domain)
		}
	}
	return dom, true, nil
}

// loadGatewayDomainConfig reads the hoster-domain list + custom flag straight
// from the settings store. Mirrors SettingsHandler.LoadGatewayDomainConfig
// but inlined here so GatewayHandler doesn't need a SettingsHandler reference.
func (h *GatewayHandler) loadGatewayDomainConfig() ([]HosterDomain, bool, string) {
	raw, _ := h.state.Store.GetSetting("gateway_hoster_domains")
	var hosters []HosterDomain
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &hosters)
	}
	enabled, _ := h.state.Store.GetSetting("gateway_custom_domains_enabled")
	cname, _ := h.state.Store.GetSetting("gateway_cname_target")
	return hosters, enabled == "true", cname
}

// loadBlockedRoutePrefixes returns the reserved leftmost-label set as a lookup
// map. Unset (raw == "") falls back to the protective default shared with the
// settings handler; an explicitly-saved list (even empty) is honored as-is.
func (h *GatewayHandler) loadBlockedRoutePrefixes() map[string]bool {
	raw, _ := h.state.Store.GetSetting("gateway_blocked_route_prefixes")
	var list []string
	if raw == "" {
		list = defaultBlockedRoutePrefixes
	} else {
		_ = json.Unmarshal([]byte(raw), &list)
	}
	m := make(map[string]bool, len(list))
	for _, p := range list {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			m[p] = true
		}
	}
	return m
}

// DeleteServerRoute DELETE /api/servers/{id}/routes/{domain} - queues removal
// of one of this server's routes. Ownership is enforced at the route by
// network.write; this only verifies the domain really belongs to the server
// named in the path.
func (h *GatewayHandler) DeleteServerRoute(w http.ResponseWriter, r *http.Request) {
	serverID := mustAtoi(mux.Vars(r)["id"])
	domain := mux.Vars(r)["domain"]

	if domain == "" {
		http.Error(w, "domain required", http.StatusBadRequest)
		return
	}

	// Ownership is enforced at the route via RequireCap(network.write) (Phase 4
	// Task 11); this only validates the route belongs to this server. For admins
	// too: the capability was resolved against {id}, so skipping this let a
	// platform server's id delete any route, a customer's included. Deleting any
	// route by name is DELETE /api/gateway/routes/{domain} (topology.write).
	server, serr := h.state.Store.GetServerByID(serverID)
	if serr != nil || server == nil {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	all := services.GetRoutesFromRedis(h.ctx(), h.state.Redis)
	found := false
	for _, rt := range all {
		// The shared predicate, not a narrower copy: a route written before
		// server_uuid was persisted carries only target_ip, and checking one
		// field answered "not found" for a route that is plainly this server's.
		if rt.Domain == domain && services.RouteBelongsToServer(rt, server.UUID) {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "Route not found for this server", http.StatusNotFound)
		return
	}

	if err := h.state.Gateway.DeleteRoute(domain); err != nil {
		http.Error(w, "Failed to delete route", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Route deletion queued"})
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
