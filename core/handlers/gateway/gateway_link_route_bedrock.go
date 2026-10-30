package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// Bedrock on a route-only address. The edge and the bridge already read only
// the domain (sys:route:editions) and a key name (the bridge's
// floodgateSubject), so what a server route gets from its node, a route-only
// customer does by hand: they download the address's Floodgate key and put it
// on their server.

const bedrockNoPlatformDomain = "Bedrock needs an address on one of the platform's domains."

type linkRouteBedrockView struct {
	Java    bool `json:"java"`
	Bedrock bool `json:"bedrock"`
	// HasKey: the address has a Floodgate key to download.
	HasKey bool `json:"has_key"`
	// Refusal is why Bedrock cannot be ADDED now; empty when it can.
	Refusal string `json:"refusal,omitempty"`
}

// linkRouteBedrockRefusal is why a route-only address cannot be given Bedrock
// right now, or "" when it can: its region, read off the edge wildcard the
// domain sits under, must have a live Bedrock entry. entries is passed in so a
// list asks Redis once.
func linkRouteBedrockRefusal(domain string, edges []services.GatewayEdgeInfo, entries []services.BedrockEntry) string {
	region, ok := services.VoiceRegionFor(domain, edges)
	if !ok {
		return bedrockNoPlatformDomain
	}
	for _, e := range entries {
		if e.Region == region {
			return ""
		}
	}
	return bedrockNoEntry
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
		handlers.SendJSONError(w, `Invalid request: expected {"java": true|false, "bedrock": true|false}`, http.StatusBadRequest)
		return
	}
	if !*req.Java && !*req.Bedrock {
		handlers.SendJSONErrorCode(w, "A route needs Java, Bedrock or both.", "edition_invalid", http.StatusBadRequest)
		return
	}
	if !isAdmin && !h.ownsCoreRoute(userID, domain) {
		handlers.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	// The stored row decides, not only the live entry ownsCoreRoute also
	// accepts: the key and the edition belong to the row's owner.
	rt, err := h.coreRoute(domain)
	if err != nil || rt == nil || (!isAdmin && rt.OwnerID != userID) {
		handlers.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	prior, err := h.state.Store.GetLinkRouteEdition(domain)
	if err != nil {
		log.Printf("bedrock: link route %s: %v", domain, err)
		handlers.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if *req.Bedrock && (prior == nil || !prior.Bedrock) {
		if !h.bedrockAllowed(r) {
			handlers.SendJSONErrorCode(w, bedrockOff, "bedrock_disabled", http.StatusConflict)
			return
		}
		if msg := handlers.OwnerRefusal(h.state, r, rt.OwnerID); msg != "" {
			handlers.SendJSONError(w, msg, http.StatusForbidden)
			return
		}
		if !h.state.GatewayEnabled() || h.state.Redis == nil {
			handlers.GatewayDisabledResponse(w)
			return
		}
		entries, err := services.LiveBedrockEntries(ctx, h.state.Redis)
		if err != nil {
			log.Printf("bedrock: link route %s: %v", domain, err)
			handlers.SendJSONError(w, "Could not check the Bedrock entries. Try again.", http.StatusBadGateway)
			return
		}
		if refusal := linkRouteBedrockRefusal(domain, services.GetEdgesFromRedis(ctx, h.state.Redis), entries); refusal != "" {
			handlers.SendJSONErrorCode(w, refusal, "bedrock_unavailable", http.StatusConflict)
			return
		}
	}
	if prior == nil && !*req.Bedrock {
		// Java only and never given Bedrock: that is the default, nothing to store.
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "bedrock": linkRouteBedrockView{Java: true}})
		return
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		handlers.SendJSONError(w, "Failed to create the Floodgate key", http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.SetLinkRouteEdition(domain, *req.Java, *req.Bedrock, key); err != nil {
		log.Printf("bedrock: link route %s: %v", domain, err)
		handlers.SendJSONError(w, "Failed to save the route", http.StatusInternalServerError)
		return
	}
	if err := h.bedrockService().PublishEditions(ctx, services.BedrockActive(r.Context(), h.state.FeatureFlags)); err != nil {
		log.Printf("bedrock: link route %s: publish editions: %v", domain, err)
		handlers.SendJSONError(w, "The route was saved, but the gateway could not be told yet. It applies within a minute.", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
		"bedrock": linkRouteBedrockView{Java: *req.Java, Bedrock: *req.Bedrock, HasKey: true}})
}

// GetLinkRouteFloodgateKey GET /api/gateway/link-routes/{domain}/floodgate-key
// - the address's Floodgate key as key.pem (the 16 raw bytes Floodgate reads).
// Owner-scoped; a key the owner holds is theirs to download again.
func (h *GatewayHandler) GetLinkRouteFloodgateKey(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)
	domain := mux.Vars(r)["domain"]
	if !isAdmin && !h.ownsCoreRoute(userID, domain) {
		handlers.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	e, err := h.state.Store.GetLinkRouteEdition(domain)
	if err != nil {
		log.Printf("bedrock: link route %s: key: %v", domain, err)
		handlers.SendJSONError(w, "Failed to read the Floodgate key", http.StatusInternalServerError)
		return
	}
	if e != nil && !isAdmin && e.OwnerID != userID {
		handlers.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	if e == nil {
		handlers.SendJSONError(w, "This address has no Floodgate key yet. Give it Bedrock first.", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="key.pem"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(e.Key)))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(e.Key)
}

// linkRouteBedrockViews is the Bedrock part of the route-only list: each
// address's stored editions and, when Bedrock cannot be added, why.
func (h *GatewayHandler) linkRouteBedrockViews(ctx context.Context, domains []string) map[string]linkRouteBedrockView {
	stored := map[string]store.LinkRouteEdition{}
	if rows, err := h.state.Store.ListLinkRouteEditions(); err == nil {
		for _, e := range rows {
			stored[e.Domain] = e
		}
	} else {
		log.Printf("bedrock: list link route editions: %v", err)
	}
	edges := services.GetEdgesFromRedis(ctx, h.state.Redis)
	entries, err := services.LiveBedrockEntries(ctx, h.state.Redis)
	if err != nil {
		log.Printf("bedrock: link routes: %v", err)
	}
	out := make(map[string]linkRouteBedrockView, len(domains))
	for _, d := range domains {
		v := linkRouteBedrockView{Java: true}
		if e, ok := stored[d]; ok {
			v = linkRouteBedrockView{Java: e.Java, Bedrock: e.Bedrock, HasKey: true}
		}
		if err != nil {
			v.Refusal = "Could not check the Bedrock entries. Try again."
		} else {
			v.Refusal = linkRouteBedrockRefusal(d, edges, entries)
		}
		out[d] = v
	}
	return out
}
