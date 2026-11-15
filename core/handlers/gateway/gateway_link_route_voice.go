package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/guard"

	"dylaris-core/handlers/httpx"
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

// Host is the name the voice mod is told (no port): our regional voice name
// for an address on the customer's own domain, else the address itself.
type linkRouteVoiceView struct {
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	TargetPort int    `json:"target_port,omitempty"`
}

func newLinkRouteVoiceView(v *store.LinkRouteVoice) linkRouteVoiceView {
	if v == nil {
		return linkRouteVoiceView{}
	}
	host := v.Host
	if host == "" {
		host = v.Domain
	}
	return linkRouteVoiceView{Enabled: true, Host: host, Port: v.Port, TargetPort: v.TargetPort}
}

// routeLookupFailed answers a route table that could not be read: a server
// error, never "Route not found", so no caller reads a database fault as the
// address being gone.
func routeLookupFailed(w http.ResponseWriter, domain string, err error) {
	log.Printf("gateway: route %s: lookup: %v", domain, err)
	httpx.SendJSONError(w, "Could not read the route. Try again.", http.StatusInternalServerError)
}

// ownsCoreRouteOrFail is ownsCoreRoute for a handler: false after it has
// answered 404 (not the caller's) or 500 (could not be read).
func (h *GatewayHandler) ownsCoreRouteOrFail(w http.ResponseWriter, userID, domain string) bool {
	owned, err := h.coreRouteOwned(userID, domain)
	switch {
	case owned:
		return true
	case err != nil:
		routeLookupFailed(w, domain, err)
	default:
		httpx.SendJSONError(w, "Route not found", http.StatusNotFound)
	}
	return false
}

func (h *GatewayHandler) coreRoute(domain string) (*store.CoreLinkRoute, error) {
	rows, err := h.d.Store.ListCoreLinkRoutes()
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
	allowed, _, err := services.ResolveVoiceAllowed(h.d.Store, &models.Server{OwnerID: ownerID})
	if err != nil {
		log.Printf("voice: route owner %s: permission check: %v", ownerID, err)
		return false
	}
	return allowed
}

const voiceNotAllowedText = "Voice chat is not available for this account."

// linkRouteVoiceRefused is the refusal of turning voice on for ownerID's
// route, "" when allowed: the rules deny it and the managed exemption does not
// apply. The exemption overrides role rows and the global switch, never an
// explicit user-scope deny. A failed read (of the rows or of the managed plan,
// where the plan would decide) is err, never a refusal: a caller must be able
// to tell "not allowed" from "could not check". It allows nothing either way.
func (h *GatewayHandler) linkRouteVoiceRefused(r *http.Request, ownerID string) (string, error) {
	allowed, by, err := services.ResolveVoiceAllowed(h.d.Store, &models.Server{OwnerID: ownerID})
	if err != nil {
		return voiceNotAllowedText, fmt.Errorf("permission check: %w", err)
	}
	switch {
	case allowed:
		return "", nil
	case by == "user:"+ownerID:
		return voiceNotAllowedText, nil
	case managedAPICall(r, ownerID):
		return "", nil
	}
	return planNeededRefusal(r, ownerID, voiceNotAllowedText), managedLookupErr(r, ownerID)
}

// SetLinkRouteVoice PUT /api/gateway/link-routes/{domain}/voice - body
// {"enabled": bool, "target_port": int}. target_port is the voice mod's port on
// the route's target host, default 24454. Off is never refused, as for a
// server. An address on the customer's own domain gets voice once its DNS
// points at one of our regions, and its mod is told OUR regional voice name
// (the answer's host).
func (h *GatewayHandler) SetLinkRouteVoice(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)
	domain := mux.Vars(r)["domain"]
	if !isAdmin && !h.ownsCoreRouteOrFail(w, userID, domain) {
		return
	}
	var req struct {
		Enabled    *bool `json:"enabled"`
		TargetPort int   `json:"target_port"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Enabled == nil {
		httpx.SendJSONError(w, `Invalid request: expected {"enabled": true|false, "target_port": 1-65535}`, http.StatusBadRequest)
		return
	}
	if req.TargetPort == 0 {
		req.TargetPort = services.VoiceInternalPort
	}
	if req.TargetPort < 1 || req.TargetPort > 65535 {
		httpx.SendJSONError(w, "The voice port must be between 1 and 65535.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if !*req.Enabled {
		prior, err := h.d.Store.GetLinkRouteVoice(domain)
		if err == nil && prior != nil {
			_, err = h.d.Store.DeleteLinkRouteVoice(domain)
			if err == nil {
				if uerr := services.UnpublishLinkRouteVoice(ctx, h.d.Redis, *prior); uerr != nil {
					log.Printf("voice: route %s: %v", domain, uerr) // the reconcile sweeps it
				}
			}
		}
		if err != nil {
			log.Printf("voice: route %s: disable: %v", domain, err)
			httpx.SendJSONError(w, "Failed to turn voice chat off", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": newLinkRouteVoiceView(nil)})
		return
	}

	if !services.GatewayRoutingEnabled(h.d.Store) {
		gate.GatewayDisabledResponse(w)
		return
	}
	rt, err := h.coreRoute(domain)
	if err != nil {
		routeLookupFailed(w, domain, err)
		return
	}
	if rt == nil {
		httpx.SendJSONError(w, "Route not found", http.StatusNotFound)
		return
	}
	if rt.SuspendedAt != nil {
		httpx.SendJSONError(w, services.ErrRouteSuspended.Error(), http.StatusConflict)
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
	if refusal, err := h.linkRouteVoiceRefused(r, rt.OwnerID); err != nil {
		log.Printf("voice: route %s: owner %s: %v", domain, rt.OwnerID, err)
		httpx.SendJSONError(w, "Could not check whether voice chat is allowed. Try again.", http.StatusInternalServerError)
		return
	} else if refusal != "" {
		httpx.SendJSONErrorCode(w, refusal, "voice_not_allowed", http.StatusConflict)
		return
	}
	if ok, err := services.LinkAnnouncesCap(ctx, h.d.Redis, rt.LinkToken, linkCapVoice); err != nil || !ok {
		httpx.SendJSONErrorCode(w, "The link of this route has not reported that it can carry voice chat. Make sure it is running and up to date, then try again.", "link_without_voice", http.StatusConflict)
		return
	}
	prior, err := h.d.Store.GetLinkRouteVoice(domain)
	if err != nil {
		log.Printf("voice: route %s: %v", domain, err)
		httpx.SendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	region, host, ok := h.linkRouteVoiceRegion(ctx, w, r, rt, prior)
	if !ok {
		return
	}
	relays, err := services.LiveVoiceRelays(ctx, h.d.Redis)
	if err != nil {
		log.Printf("voice: route %s: %v", domain, err)
		httpx.SendJSONError(w, "Could not check the voice relays. Try again.", http.StatusBadGateway)
		return
	}
	relayUp := false
	for _, rl := range relays {
		relayUp = relayUp || rl.Region == region
	}
	if !relayUp {
		httpx.SendJSONErrorCode(w, "No voice relay is running in this address's region yet.", "no_relay", http.StatusConflict)
		return
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		httpx.SendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	got, err := h.d.Store.AllocateLinkRouteVoice(domain, region, host, req.TargetPort, key, services.VoicePortMin, services.VoicePortMax)
	if errors.Is(err, store.ErrVoicePoolFull) {
		httpx.SendJSONErrorCode(w, "Every voice port in this region is taken.", "voice_pool_full", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("voice: route %s: allocate: %v", domain, err)
		httpx.SendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	if prior != nil && (prior.Region != got.Region || prior.Port != got.Port) {
		if err := services.UnpublishLinkRouteVoice(ctx, h.d.Redis, *prior); err != nil {
			log.Printf("voice: route %s: %v", domain, err)
		}
	}
	if err := services.PublishLinkRouteVoice(ctx, h.d.Redis, got); err != nil {
		log.Printf("voice: route %s: %v", domain, err)
		if prior == nil {
			if _, derr := h.d.Store.DeleteLinkRouteVoice(domain); derr != nil {
				log.Printf("voice: route %s: roll back: %v", domain, derr)
			}
		}
		httpx.SendJSONError(w, "Voice chat could not be published to the gateway. Try again.", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": newLinkRouteVoiceView(&got)})
}

// linkRouteVoiceRegion picks the relay region and the host the mod is told,
// or answers the refusal and returns false. In order: an address on one of our
// domains is today's rule (no host); an address that already has voice keeps
// its region and host WITHOUT DNS, so a later DNS change can never turn the
// 7b-2 sweep's re-assert into a refusal; only a NEW own-domain address asks
// the pointing check. The name is the stored route's, never request text, and
// the caller has already checked the route is the caller's own.
func (h *GatewayHandler) linkRouteVoiceRegion(ctx context.Context, w http.ResponseWriter, r *http.Request, rt *store.CoreLinkRoute, prior *store.LinkRouteVoice) (region, host string, ok bool) {
	domain := rt.Domain
	edges := services.GetEdgesFromRedis(ctx, h.d.Redis)
	if region, ok := services.VoiceRegionFor(domain, edges); ok {
		return region, "", true
	}
	if prior != nil {
		return prior.Region, prior.Host, true
	}
	// A nil checker answers lookup_failed.
	p := h.d.Pointing.CheckFor(ctx, rt.OwnerID, domain, services.PurposeVoiceRegion, h.pointingAllow(r))
	if p.Status != services.PointingOK {
		h.voiceRegionRefusal(w, p, domain)
		return "", "", false
	}
	host, found := services.VoiceHostFor(p.Region, edges)
	if !found {
		h.voiceRegionRefusal(w, p, domain)
		return "", "", false
	}
	return p.Region, host, true
}

// voiceRegionRefusal answers a voice-region pointing check of domain that gave
// no region: rate_limited 429, any unknown 503 dns_lookup_failed (nothing was
// changed, try again), a known "no" 409 no_routes with its instruction, and an
// ok whose region has no online edge left 409 no_routes.
func (h *GatewayHandler) voiceRegionRefusal(w http.ResponseWriter, p services.Pointing, domain string) {
	switch {
	case p.Status == services.PointingRateLimited:
		httpx.SendJSONErrorCode(w, services.PointingUnknownText(p, domain), "rate_limited", http.StatusTooManyRequests)
	case !p.Known():
		httpx.SendJSONErrorCode(w, services.PointingUnknownText(p, domain), "dns_lookup_failed", http.StatusServiceUnavailable)
	case p.Status == services.PointingOK:
		httpx.SendJSONErrorCode(w, "No edge of this domain's region is online right now. Try again in a moment.", "no_routes", http.StatusConflict)
	default:
		hosters, _, cname := h.loadGatewayDomainConfig()
		bases := make([]string, 0, len(hosters))
		for _, hd := range hosters {
			bases = append(bases, hd.Domain)
		}
		httpx.SendJSONErrorCode(w, services.PointingRefusal(p, domain, services.CNAMETargets(cname, bases), services.PurposeVoiceRegion),
			"no_routes", http.StatusConflict)
	}
}
