package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// Voice chat on a route-only address. Core allocates the public port and the
// uplink key; the customer's link fetches them (LinkVoice), registers with the
// relay and forwards to the voice mod on the route's target host. Core cannot
// write the mod's config on the customer's machine, so the panel shows the
// line to set by hand.

// linkCapVoice is what a link announces when it carries route-only voice.
const linkCapVoice = "voice"

type linkRouteVoiceView struct {
	Enabled    bool `json:"enabled"`
	Port       int  `json:"port,omitempty"`
	TargetPort int  `json:"target_port,omitempty"`
}

func newLinkRouteVoiceView(v *store.LinkRouteVoice) linkRouteVoiceView {
	if v == nil {
		return linkRouteVoiceView{}
	}
	return linkRouteVoiceView{Enabled: true, Port: v.Port, TargetPort: v.TargetPort}
}

func (h *GatewayHandler) coreRoute(domain string) (*store.CoreLinkRoute, error) {
	rows, err := h.state.Store.ListCoreLinkRoutes()
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Domain == domain {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// linkRouteVoiceAllowed is ResolveVoiceAllowed for a route's owner, failing
// closed. A route has no server, so the server scope never has a row.
func (h *GatewayHandler) linkRouteVoiceAllowed(ownerID string) bool {
	allowed, _, err := services.ResolveVoiceAllowed(h.state.Store, &models.Server{OwnerID: ownerID})
	if err != nil {
		log.Printf("voice: route owner %s: permission check: %v", ownerID, err)
		return false
	}
	return allowed
}

// SetLinkRouteVoice PUT /api/gateway/link-routes/{domain}/voice - body
// {"enabled": bool, "target_port": int}. target_port is the voice mod's port on
// the route's target host, default 24454. Off is never refused, as for a
// server.
func (h *GatewayHandler) SetLinkRouteVoice(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)
	domain := mux.Vars(r)["domain"]
	if !isAdmin && !h.ownsCoreRoute(userID, domain) {
		handlers.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	var req struct {
		Enabled    *bool `json:"enabled"`
		TargetPort int   `json:"target_port"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Enabled == nil {
		handlers.SendJSONError(w, `Invalid request: expected {"enabled": true|false, "target_port": 1-65535}`, http.StatusBadRequest)
		return
	}
	if req.TargetPort == 0 {
		req.TargetPort = services.VoiceInternalPort
	}
	if req.TargetPort < 1 || req.TargetPort > 65535 {
		handlers.SendJSONError(w, "The voice port must be between 1 and 65535.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if !*req.Enabled {
		prior, err := h.state.Store.GetLinkRouteVoice(domain)
		if err == nil && prior != nil {
			_, err = h.state.Store.DeleteLinkRouteVoice(domain)
			if err == nil {
				if uerr := services.UnpublishLinkRouteVoice(ctx, h.state.Redis, *prior); uerr != nil {
					log.Printf("voice: route %s: %v", domain, uerr) // the reconcile sweeps it
				}
			}
		}
		if err != nil {
			log.Printf("voice: route %s: disable: %v", domain, err)
			handlers.SendJSONError(w, "Failed to turn voice chat off", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": newLinkRouteVoiceView(nil)})
		return
	}

	if !h.state.GatewayEnabled() {
		handlers.GatewayDisabledResponse(w)
		return
	}
	rt, err := h.coreRoute(domain)
	if err != nil || rt == nil {
		handlers.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	if msg := handlers.OwnerRefusal(h.state, r, rt.OwnerID); msg != "" {
		handlers.SendJSONError(w, msg, http.StatusForbidden)
		return
	}
	if !h.linkRouteVoiceAllowed(rt.OwnerID) {
		handlers.SendJSONErrorCode(w, "Voice chat is not available for this account.", "voice_not_allowed", http.StatusConflict)
		return
	}
	if ok, err := handlers.LinkAnnouncesCap(ctx, h.state.Redis, rt.LinkToken, linkCapVoice); err != nil || !ok {
		handlers.SendJSONErrorCode(w, "The link of this route has not reported that it can carry voice chat. Make sure it is running and up to date, then try again.", "link_without_voice", http.StatusConflict)
		return
	}
	region, ok := services.VoiceRegionFor(domain, services.GetEdgesFromRedis(ctx, h.state.Redis))
	if !ok {
		handlers.SendJSONErrorCode(w, "Voice chat needs an address on one of the platform's domains.", "no_routes", http.StatusConflict)
		return
	}
	relays, err := services.LiveVoiceRelays(ctx, h.state.Redis)
	if err != nil {
		log.Printf("voice: route %s: %v", domain, err)
		handlers.SendJSONError(w, "Could not check the voice relays. Try again.", http.StatusBadGateway)
		return
	}
	relayUp := false
	for _, rl := range relays {
		relayUp = relayUp || rl.Region == region
	}
	if !relayUp {
		handlers.SendJSONErrorCode(w, "No voice relay is running in this address's region yet.", "no_relay", http.StatusConflict)
		return
	}

	prior, err := h.state.Store.GetLinkRouteVoice(domain)
	if err != nil {
		log.Printf("voice: route %s: %v", domain, err)
		handlers.SendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		handlers.SendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	got, err := h.state.Store.AllocateLinkRouteVoice(domain, region, req.TargetPort, key, services.VoicePortMin, services.VoicePortMax)
	if errors.Is(err, store.ErrVoicePoolFull) {
		handlers.SendJSONErrorCode(w, "Every voice port in this region is taken.", "voice_pool_full", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("voice: route %s: allocate: %v", domain, err)
		handlers.SendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	if prior != nil && (prior.Region != got.Region || prior.Port != got.Port) {
		if err := services.UnpublishLinkRouteVoice(ctx, h.state.Redis, *prior); err != nil {
			log.Printf("voice: route %s: %v", domain, err)
		}
	}
	if err := services.PublishLinkRouteVoice(ctx, h.state.Redis, got); err != nil {
		log.Printf("voice: route %s: %v", domain, err)
		if prior == nil {
			if _, derr := h.state.Store.DeleteLinkRouteVoice(domain); derr != nil {
				log.Printf("voice: route %s: roll back: %v", domain, derr)
			}
		}
		handlers.SendJSONError(w, "Voice chat could not be published to the gateway. Try again.", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": newLinkRouteVoiceView(&got)})
}
