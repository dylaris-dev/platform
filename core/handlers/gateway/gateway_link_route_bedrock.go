package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// Bedrock on a route-only address. The edge and the bridge already read only
// the domain (sys:route:editions) and a key name (the bridge's
// floodgateSubject), so what a server route gets from its node, a route-only
// customer does by hand: they download the address's Floodgate key and put it
// on their server. An address on the customer's own domain gets Bedrock once
// its DNS points at one of our regions: the check runs only when Bedrock is
// ADDED, and its verdict is stored for the list to show.

type linkRouteBedrockView struct {
	Java    bool `json:"java"`
	Bedrock bool `json:"bedrock"`
	// HasKey: the address has a Floodgate key to download.
	HasKey bool `json:"has_key"`
	// Refusal is why Bedrock cannot be ADDED now; empty when it can.
	Refusal string `json:"refusal,omitempty"`
	// The last stored DNS check of an address on the customer's own domain:
	// "" and null for an address on one of our domains and for one never
	// checked. Display only: the add always checks afresh.
	DNSStatus       string     `json:"dns_status"`
	DNSCheckedAt    *time.Time `json:"dns_checked_at"`
	DNSFailingSince *time.Time `json:"dns_failing_since"`
}

// withDNS copies a stored check into v.
func (v linkRouteBedrockView) withDNS(c store.AddressDNSCheck) linkRouteBedrockView {
	at := c.CheckedAt
	v.DNSStatus, v.DNSCheckedAt, v.DNSFailingSince = c.Status, &at, c.FailingSince
	return v
}

func hasBedrockEntry(region string, entries []services.BedrockEntry) bool {
	for _, e := range entries {
		if e.Region == region {
			return true
		}
	}
	return false
}

// linkRouteBedrockRefusal is why a route-only address cannot be given Bedrock
// right now, or "" when it can: its region, read off the edge wildcard the
// domain sits under, must have a live Bedrock entry. Callers pass only a
// regional domain. entries is passed in so a list asks Redis once.
func linkRouteBedrockRefusal(domain string, edges []services.GatewayEdgeInfo, entries []services.BedrockEntry) string {
	region, _ := services.VoiceRegionFor(domain, edges)
	if hasBedrockEntry(region, entries) {
		return ""
	}
	return bedrockNoEntry
}

// bedrockOwnDomainCheck is the add check of an address on the customer's own
// domain, for route-only and hosted Bedrock alike: the pointing check, its
// verdict stored (a refusal too, so the list can show it), and the region it
// points at must have a live entry. domain must be a route the caller already
// admitted as its own; owner bounds the lookups one owner runs at once. code
// "" means Bedrock may be added, with the stored check. "Could not check"
// (dns_lookup_failed 503, rate_limited 429) stores and changes nothing.
func (h *GatewayHandler) bedrockOwnDomainCheck(ctx context.Context, r *http.Request, owner, domain string, entries []services.BedrockEntry) (stored store.AddressDNSCheck, code string, status int, msg string) {
	// A nil checker answers lookup_failed.
	p := h.d.Pointing.CheckFor(ctx, owner, domain, services.PurposeBedrock, h.pointingAllow(r))
	switch {
	case p.Status == services.PointingRateLimited:
		return stored, "rate_limited", http.StatusTooManyRequests, services.PointingUnknownText(p, domain)
	case !p.Known():
		return stored, "dns_lookup_failed", http.StatusServiceUnavailable, services.PointingUnknownText(p, domain)
	}
	ok := p.Status == services.PointingOK
	stored, err := h.d.Store.RecordAddressDNSCheck(domain, string(p.Status), p.Region, ok)
	if err != nil {
		// Display state only: the verdict itself still decides.
		log.Printf("bedrock: %s: store dns check: %v", domain, err)
		stored = store.AddressDNSCheck{Name: domain, Status: string(p.Status), Region: p.Region, CheckedAt: time.Now()}
	}
	if !ok {
		return store.AddressDNSCheck{}, "bedrock_unavailable", http.StatusConflict,
			services.PointingRefusal(p, domain, h.pointingCNAMETargets(), services.PurposeBedrock)
	}
	if !hasBedrockEntry(p.Region, entries) {
		return store.AddressDNSCheck{}, "bedrock_unavailable", http.StatusConflict, bedrockNoEntry
	}
	return stored, "", http.StatusOK, ""
}

// pointingCNAMETargets is where a refusal tells the customer to point their
// CNAME: the operator's label under each hoster domain.
func (h *GatewayHandler) pointingCNAMETargets() []string {
	hosters, _, cname := h.loadGatewayDomainConfig()
	bases := make([]string, 0, len(hosters))
	for _, hd := range hosters {
		bases = append(bases, hd.Domain)
	}
	return services.CNAMETargets(cname, bases)
}

// SetLinkRouteBedrock PUT /api/gateway/link-routes/{domain}/bedrock - body
// {"java": bool, "bedrock": bool}. Owner-scoped like the voice switch beside
// it. Adding Bedrock is refused while the caller may not or the region has no
// entry; taking it away or changing Java never is.
func (h *GatewayHandler) SetLinkRouteBedrock(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)
	domain := mux.Vars(r)["domain"]
	var req struct {
		Java    *bool `json:"java"`
		Bedrock *bool `json:"bedrock"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Java == nil || req.Bedrock == nil {
		httpx.SendJSONError(w, `Invalid request: expected {"java": true|false, "bedrock": true|false}`, http.StatusBadRequest)
		return
	}
	if !*req.Java && !*req.Bedrock {
		httpx.SendJSONErrorCode(w, "A route needs Java, Bedrock or both.", "edition_invalid", http.StatusBadRequest)
		return
	}
	if !isAdmin && !h.ownsCoreRouteOrFail(w, userID, domain) {
		return
	}
	// The stored row decides, not only the live entry coreRouteOwned also
	// accepts: the key and the edition belong to the row's owner.
	rt, err := h.coreRoute(domain)
	if err != nil {
		routeLookupFailed(w, domain, err)
		return
	}
	if rt == nil || (!isAdmin && rt.OwnerID != userID) {
		httpx.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	prior, err := h.d.Store.GetLinkRouteEdition(domain)
	if err != nil {
		log.Printf("bedrock: link route %s: %v", domain, err)
		httpx.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// Set only when this request ran the own-domain check (an ADD).
	var dnsCheck store.AddressDNSCheck
	checked := false
	if *req.Bedrock && (prior == nil || !prior.Bedrock) {
		allowed, settingErr := h.bedrockAllowedChecked(r)
		if !allowed && !managedAPICall(r, rt.OwnerID) {
			if settingErr != nil {
				bedrockSettingUnreadable(w, settingErr)
				return
			}
			if err := managedLookupErr(r, rt.OwnerID); err != nil {
				log.Printf("bedrock: link route %s: owner %s: %v", domain, rt.OwnerID, err)
				httpx.SendJSONError(w, "Could not check whether Bedrock is allowed. Try again.", http.StatusInternalServerError)
				return
			}
			httpx.SendJSONErrorCode(w, planNeededRefusal(r, rt.OwnerID, bedrockOff), "bedrock_disabled", http.StatusConflict)
			return
		}
		if msg := guard.OwnerRefusal(h.d.Store, h.d.SuspendGrace, r, rt.OwnerID); msg != "" {
			httpx.SendJSONError(w, msg, http.StatusForbidden)
			return
		}
		if h.linkTokenRetiring(rt.OwnerID, rt.LinkToken) {
			httpx.SendJSONErrorCode(w, kitRetiringMessage, "kit_retiring", http.StatusConflict)
			return
		}
		if !services.GatewayRoutingEnabled(h.d.Store) || h.d.Redis == nil {
			gate.GatewayDisabledResponse(w)
			return
		}
		entries, err := services.LiveBedrockEntries(ctx, h.d.Redis)
		if err != nil {
			log.Printf("bedrock: link route %s: %v", domain, err)
			httpx.SendJSONError(w, "Could not check the Bedrock entries. Try again.", http.StatusBadGateway)
			return
		}
		edges := services.GetEdgesFromRedis(ctx, h.d.Redis)
		if _, regional := services.VoiceRegionFor(domain, edges); regional {
			if refusal := linkRouteBedrockRefusal(domain, edges, entries); refusal != "" {
				httpx.SendJSONErrorCode(w, refusal, "bedrock_unavailable", http.StatusConflict)
				return
			}
		} else {
			var code, msg string
			var status int
			if dnsCheck, code, status, msg = h.bedrockOwnDomainCheck(ctx, r, rt.OwnerID, rt.Domain, entries); code != "" {
				httpx.SendJSONErrorCode(w, msg, code, status)
				return
			}
			checked = true
		}
	}
	if prior == nil && !*req.Bedrock {
		// Java only and never given Bedrock: that is the default, nothing to store.
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "bedrock": linkRouteBedrockView{Java: true}})
		return
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		httpx.SendJSONError(w, "Failed to create the Floodgate key", http.StatusInternalServerError)
		return
	}
	if err := h.d.Store.SetLinkRouteEdition(domain, *req.Java, *req.Bedrock, key); err != nil {
		log.Printf("bedrock: link route %s: %v", domain, err)
		httpx.SendJSONError(w, "Failed to save the route", http.StatusInternalServerError)
		return
	}
	if err := h.bedrockService().PublishEditions(ctx, services.BedrockActive(r.Context(), h.d.FeatureFlags)); err != nil {
		log.Printf("bedrock: link route %s: publish editions: %v", domain, err)
		httpx.SendJSONError(w, "The route was saved, but the gateway could not be told yet. It applies within a minute.", http.StatusBadGateway)
		return
	}
	view := linkRouteBedrockView{Java: *req.Java, Bedrock: *req.Bedrock, HasKey: true}
	if checked {
		view = view.withDNS(dnsCheck)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "bedrock": view})
}

// GetLinkRouteFloodgateKey GET /api/gateway/link-routes/{domain}/floodgate-key
// - the address's Floodgate key as key.pem (the 16 raw bytes Floodgate reads).
// Owner-scoped; a key the owner holds is theirs to download again.
func (h *GatewayHandler) GetLinkRouteFloodgateKey(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)
	key, status, msg := h.linkRouteFloodgateKey(userID, isAdmin, mux.Vars(r)["domain"])
	if status != http.StatusOK {
		httpx.SendJSONError(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="key.pem"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(key)))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(key)
}

// linkRouteFloodgateKey is the address's Floodgate key for userID, or the
// status and message to refuse with. The panel download and the external API
// share it so both run the same ownership checks in the same order. The key
// never goes into msg or a log line.
func (h *GatewayHandler) linkRouteFloodgateKey(userID string, isAdmin bool, domain string) (key []byte, status int, msg string) {
	if !isAdmin {
		owned, err := h.coreRouteOwned(userID, domain)
		if !owned && err != nil {
			log.Printf("bedrock: link route %s: key: lookup: %v", domain, err)
			return nil, http.StatusInternalServerError, "Failed to read the Floodgate key"
		}
		if !owned {
			return nil, http.StatusNotFound, "Route not found"
		}
	}
	e, err := h.d.Store.GetLinkRouteEdition(domain)
	if err != nil {
		log.Printf("bedrock: link route %s: key: %v", domain, err)
		return nil, http.StatusInternalServerError, "Failed to read the Floodgate key"
	}
	if e != nil && !isAdmin && e.OwnerID != userID {
		return nil, http.StatusNotFound, "Route not found"
	}
	if e == nil {
		return nil, http.StatusNotFound, "This address has no Floodgate key yet. Give it Bedrock first."
	}
	return e.Key, http.StatusOK, ""
}

// linkRouteBedrockViews is the Bedrock part of the route-only list: each
// address's stored editions, its stored DNS check and, when Bedrock cannot be
// added, why. It never resolves: an own domain's refusal is "" unless the
// region its last check found has no entry (the add checks afresh anyway).
func (h *GatewayHandler) linkRouteBedrockViews(ctx context.Context, domains []string) map[string]linkRouteBedrockView {
	stored := map[string]store.LinkRouteEdition{}
	if rows, err := h.d.Store.ListLinkRouteEditions(); err == nil {
		for _, e := range rows {
			stored[e.Domain] = e
		}
	} else {
		log.Printf("bedrock: list link route editions: %v", err)
	}
	edges := services.GetEdgesFromRedis(ctx, h.d.Redis)
	entries, err := services.LiveBedrockEntries(ctx, h.d.Redis)
	if err != nil {
		log.Printf("bedrock: link routes: %v", err)
	}
	regional := make(map[string]bool, len(domains))
	var own []string
	for _, d := range domains {
		if _, ok := services.VoiceRegionFor(d, edges); ok {
			regional[d] = true
		} else {
			own = append(own, d)
		}
	}
	checks := map[string]store.AddressDNSCheck{}
	if len(own) > 0 {
		got, cerr := h.d.Store.GetAddressDNSChecks(own)
		if cerr != nil {
			log.Printf("bedrock: list dns checks: %v", cerr)
		} else {
			checks = got
		}
	}
	out := make(map[string]linkRouteBedrockView, len(domains))
	for _, d := range domains {
		v := linkRouteBedrockView{Java: true}
		if e, ok := stored[d]; ok {
			v = linkRouteBedrockView{Java: e.Java, Bedrock: e.Bedrock, HasKey: true}
		}
		c, checked := checks[strings.ToLower(d)]
		if checked && !regional[d] {
			v = v.withDNS(c)
		}
		switch {
		case err != nil:
			v.Refusal = "Could not check the Bedrock entries. Try again."
		case regional[d]:
			v.Refusal = linkRouteBedrockRefusal(d, edges, entries)
		case checked && c.Region != "" && !hasBedrockEntry(c.Region, entries):
			v.Refusal = bedrockNoEntry
		}
		out[d] = v
	}
	return out
}
