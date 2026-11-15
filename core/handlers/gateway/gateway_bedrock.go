package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/modcompat"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type bedrockRouteView struct {
	Domain  string `json:"domain"`
	Java    bool   `json:"java"`
	Bedrock bool   `json:"bedrock"`
	// OwnDomain: the address is on the customer's own domain.
	OwnDomain bool `json:"ownDomain,omitempty"`
	// The last stored DNS check of an own-domain address (Core's own in local
	// mode, the provider's in remote mode), absent until the first one. Display only: the add always checks afresh.
	DNSStatus       string     `json:"dnsStatus,omitempty"`
	DNSCheckedAt    *time.Time `json:"dnsCheckedAt,omitempty"`
	DNSFailingSince *time.Time `json:"dnsFailingSince,omitempty"`
}

type bedrockView struct {
	// Enabled: at least one route of the server has Bedrock (stored).
	Enabled bool `json:"enabled"`
	// Active: the platform switch is not off, so those routes take Bedrock now.
	Active bool `json:"active"`
	// Allowed: THIS caller may give a route Bedrock (switch all, or admin and
	// the caller is a panel admin).
	Allowed   bool   `json:"allowed"`
	Available bool   `json:"available"`
	Refusal   string `json:"refusal,omitempty"`
	Address   string `json:"address"`
	Loader    string `json:"loader"`
	// Routes lists the routes that differ from Java only, and in local mode
	// also the own-domain routes, so their DNS state can be shown.
	Routes []bedrockRouteView `json:"routes"`
}

const (
	bedrockNotRouted = "This server is not routed through the gateway."
	bedrockNoEntry   = "Bedrock needs the gateway: no Bedrock entry is running in this server's region yet."
	bedrockOff       = "Bedrock is turned off on this platform."
)

// bedrockAllowed is whether this caller may give a route Bedrock.
func (h *GatewayHandler) bedrockAllowed(r *http.Request) bool {
	ok, _ := h.bedrockAllowedChecked(r)
	return ok
}

// bedrockAllowedChecked is bedrockAllowed with a setting that could not be
// read as an error: false and an error must answer a server error, never
// "Bedrock is turned off" (a customer's sweep treats that one as final).
func (h *GatewayHandler) bedrockAllowedChecked(r *http.Request) (bool, error) {
	if h.d.FeatureFlags == nil {
		return false, nil
	}
	aud, err := h.d.FeatureFlags.BedrockAudienceChecked()
	if err != nil {
		return false, fmt.Errorf("read the Bedrock setting: %w", err)
	}
	switch aud {
	case "all":
		return true, nil
	case "admin":
		return reqctx.IsAdmin(r), nil
	}
	return false, nil
}

func bedrockSettingUnreadable(w http.ResponseWriter, err error) {
	log.Printf("bedrock: %v", err)
	httpx.SendJSONError(w, "Could not check whether Bedrock is allowed. Try again.", http.StatusInternalServerError)
}

func (h *GatewayHandler) bedrockService() *services.BedrockService {
	svc := services.NewBedrockService(h.d.Store, h.d.Redis)
	svc.SetFlags(h.d.FeatureFlags)
	return svc
}

// bedrockAvailability is the server summary, without any DNS lookup. With a
// route on one of our domains, address is the voice address rule's pick (the
// lexically first route exactly one label under an online edge's wildcard)
// and its region must have a live entry. With only own-domain routes, address
// is the lexically first of them and any live entry offers Bedrock: the real
// decision is the per-address add (bedrockAddRefusal). own lists the server's
// own-domain routes, lower case and sorted.
func (h *GatewayHandler) bedrockAvailability(ctx context.Context, srv *models.Server) (address, refusal string, own []string) {
	if !services.GatewayRoutingEnabled(h.d.Store) || h.d.Redis == nil {
		return "", bedrockNotRouted, nil
	}
	routes, err := services.ReadRoutesFromRedis(ctx, h.d.Redis)
	if err != nil {
		log.Printf("bedrock: server %d: read routes: %v", srv.ID, err)
		return "", "Could not read this server's routes. Try again.", nil
	}
	edges := services.GetEdgesFromRedis(ctx, h.d.Redis)
	for _, rt := range routes {
		if !services.RouteBelongsToServer(rt, srv.UUID) {
			continue
		}
		if _, regional := services.VoiceRegionFor(rt.Domain, edges); !regional {
			own = append(own, strings.ToLower(rt.Domain))
		}
	}
	sort.Strings(own)
	host, region, regional := services.VoiceRouteFor(routes, edges, srv.UUID)
	if !regional {
		if len(own) == 0 {
			return "", bedrockNotRouted, nil
		}
		host = own[0]
	}
	entries, err := services.LiveBedrockEntries(ctx, h.d.Redis)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		return host, "Could not check the Bedrock entries. Try again.", own
	}
	for _, e := range entries {
		if !regional || e.Region == region {
			return host, "", own
		}
	}
	return host, bedrockNoEntry, own
}

// bedrockAddRefusal is the add check for ONE address of a hosted server,
// asked only when Bedrock is added to it. An address on one of our domains
// needs a live entry in its region; an own domain gets the route-only check
// (bedrockOwnDomainCheck), which resolves. code "" means it may be added.
func (h *GatewayHandler) bedrockAddRefusal(ctx context.Context, r *http.Request, owner, domain string) (code string, status int, msg string) {
	if !services.GatewayRoutingEnabled(h.d.Store) || h.d.Redis == nil {
		return "bedrock_unavailable", http.StatusConflict, bedrockNotRouted
	}
	entries, err := services.LiveBedrockEntries(ctx, h.d.Redis)
	if err != nil {
		log.Printf("bedrock: route %s: %v", domain, err)
		return "bedrock_unavailable", http.StatusConflict, "Could not check the Bedrock entries. Try again."
	}
	if region, regional := services.VoiceRegionFor(domain, services.GetEdgesFromRedis(ctx, h.d.Redis)); regional {
		if !hasBedrockEntry(region, entries) {
			return "bedrock_unavailable", http.StatusConflict, bedrockNoEntry
		}
		return "", http.StatusOK, ""
	}
	_, code, status, msg = h.bedrockOwnDomainCheck(ctx, r, owner, domain, entries)
	return code, status, msg
}

// GetServerBedrock GET /api/servers/{id}/bedrock.
func (h *GatewayHandler) GetServerBedrock(w http.ResponseWriter, r *http.Request) {
	srv, err := h.d.Store.GetServerByID(httpx.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	if rg != nil {
		h.getRemoteBedrock(w, r, rg, srv)
		return
	}
	rows, err := h.d.Store.ListRouteEditions(srv.ID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	addr, refusal, own := h.bedrockAvailability(ctx, srv)
	v := bedrockView{
		Active: services.BedrockActive(r.Context(), h.d.FeatureFlags), Allowed: h.bedrockAllowed(r),
		Available: refusal == "", Refusal: refusal, Address: addr,
		Loader: modcompat.DefaultTargetDirForLoader(srv.InstallerType), Routes: []bedrockRouteView{},
	}
	isOwn := make(map[string]bool, len(own))
	for _, d := range own {
		isOwn[d] = true
	}
	checks := map[string]store.AddressDNSCheck{}
	if len(own) > 0 {
		// Stored answers only: this view never resolves.
		if got, err := h.d.Store.GetAddressDNSChecks(own); err != nil {
			log.Printf("bedrock: server %d: dns checks: %v", srv.ID, err)
		} else {
			checks = got
		}
	}
	route := func(domain string, java, bedrock bool) bedrockRouteView {
		rv := bedrockRouteView{Domain: domain, Java: java, Bedrock: bedrock}
		d := strings.ToLower(domain)
		if rv.OwnDomain = isOwn[d]; rv.OwnDomain {
			if c, ok := checks[d]; ok {
				at := c.CheckedAt
				rv.DNSStatus, rv.DNSCheckedAt, rv.DNSFailingSince = c.Status, &at, c.FailingSince
			}
		}
		return rv
	}
	listed := map[string]bool{}
	for _, e := range rows {
		v.Enabled = v.Enabled || e.Bedrock
		v.Routes = append(v.Routes, route(e.Domain, e.Java, e.Bedrock))
		listed[strings.ToLower(e.Domain)] = true
	}
	for _, d := range own {
		if !listed[d] {
			v.Routes = append(v.Routes, route(d, true, false))
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "bedrock": v})
}

// SetRouteEdition PUT /api/servers/{id}/bedrock/routes - body
// {"domain": "...", "java": bool, "bedrock": bool}. Giving a route Bedrock is
// refused while the caller may not, or the region has no Bedrock entry;
// taking it away or changing Java never is, so an owner can always get out of
// a state. The table is written first and the gateway list after it.
func (h *GatewayHandler) SetRouteEdition(w http.ResponseWriter, r *http.Request) {
	srv, err := h.d.Store.GetServerByID(httpx.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		Domain  string `json:"domain"`
		Java    *bool  `json:"java"`
		Bedrock *bool  `json:"bedrock"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Java == nil || req.Bedrock == nil || req.Domain == "" {
		httpx.SendJSONError(w, `Invalid request: expected {"domain": "...", "java": true|false, "bedrock": true|false}`, http.StatusBadRequest)
		return
	}
	if !*req.Java && !*req.Bedrock {
		httpx.SendJSONErrorCode(w, "A route needs Java, Bedrock or both.", "edition_invalid", http.StatusBadRequest)
		return
	}
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	if rg != nil {
		h.setRemoteRouteEdition(w, r, rg, srv, req.Domain, *req.Java, *req.Bedrock)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	routes, err := services.ReadRoutesFromRedis(ctx, h.d.Redis)
	if err != nil {
		log.Printf("bedrock: server %d: read routes: %v", srv.ID, err)
		httpx.SendJSONError(w, "Could not read this server's routes. Try again.", http.StatusBadGateway)
		return
	}
	found := false
	for _, rt := range routes {
		if rt.Domain == req.Domain && services.RouteBelongsToServer(rt, srv.UUID) {
			found = true
			break
		}
	}
	if !found {
		httpx.SendJSONError(w, "Route not found for this server", http.StatusNotFound)
		return
	}
	rows, err := h.d.Store.ListRouteEditions(srv.ID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	hadBedrock := false
	for _, e := range rows {
		if e.Domain == req.Domain {
			hadBedrock = e.Bedrock
		}
	}
	if *req.Bedrock && !hadBedrock {
		if allowed, err := h.bedrockAllowedChecked(r); err != nil {
			bedrockSettingUnreadable(w, err)
			return
		} else if !allowed {
			httpx.SendJSONErrorCode(w, bedrockOff, "bedrock_disabled", http.StatusConflict)
			return
		}
		if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
			return
		}
		if code, status, msg := h.bedrockAddRefusal(ctx, r, srv.OwnerID, req.Domain); code != "" {
			httpx.SendJSONErrorCode(w, msg, code, status)
			return
		}
	}
	if err := h.d.Store.SetRouteEdition(srv.ID, req.Domain, *req.Java, *req.Bedrock); err != nil {
		log.Printf("bedrock: server %d: route %s: %v", srv.ID, req.Domain, err)
		httpx.SendJSONError(w, "Failed to save the route", http.StatusInternalServerError)
		return
	}
	if err := h.bedrockService().PublishEditions(ctx, services.BedrockActive(r.Context(), h.d.FeatureFlags)); err != nil {
		log.Printf("bedrock: server %d: publish editions: %v", srv.ID, err)
		httpx.SendJSONError(w, "The route was saved, but the gateway could not be told yet. It applies within a minute.", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
		"route": bedrockRouteView{Domain: req.Domain, Java: *req.Java, Bedrock: *req.Bedrock}})
}

const (
	remoteBedrockNoAddress   = "Bedrock needs an address on the managed gateway. Add one first."
	remoteBedrockUnreachable = "The managed gateway could not be reached. Try again."
	remoteBedrockKeyLate     = "Bedrock is on, but its key could not be handed to the server's node yet. It is retried within a minute; restart the server after that."
)

// republishMissingKey hands the server's stored Floodgate key to its node when
// Redis does not hold it; published reports that it did. No stored key is
// nothing to hand over.
func (h *GatewayHandler) republishMissingKey(ctx context.Context, srv *models.Server) (published bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := h.d.Redis.Exists(ctx, services.FloodgateKeyRedisKey(srv.UUID)).Result()
	if err != nil || n > 0 {
		return false, err
	}
	key, found, err := h.d.Store.GetBedrockKey(srv.ID)
	if err != nil || !found {
		return false, err
	}
	if err := h.bedrockService().PublishKey(ctx, srv.UUID, key); err != nil {
		return false, err
	}
	return true, nil
}

// getRemoteBedrock is GetServerBedrock on the managed gateway: the editions
// from the local columns, the refusal and the DNS state of an own-domain
// address from the provider's route list (this Core never resolves). Address
// is the server's Bedrock address, else the voice address rule's pick (a
// regional address first, else the lexically first own domain). It carries
// no key, and the panel offers no key download here.
func (h *GatewayHandler) getRemoteBedrock(w http.ResponseWriter, r *http.Request, rg *services.RemoteGateway, srv *models.Server) {
	editions, err := rg.ServerEditions(srv.UUID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// The provider holds the editions; the platform switch here does not pause
	// them, so they are always active.
	v := bedrockView{Active: true, Allowed: h.bedrockAllowed(r),
		Loader: modcompat.DefaultTargetDirForLoader(srv.InstallerType), Routes: []bedrockRouteView{}}
	domains := make([]string, 0, len(editions))
	for d := range editions {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	own, err := rg.ServerOwnDomains(srv.UUID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	for _, d := range domains {
		ed := editions[d]
		if ed == "" && !own[d] {
			continue
		}
		// Own-domain addresses are listed with Java only too, so their DNS
		// state can be shown before Bedrock is added.
		rv := bedrockRouteView{Domain: d, Java: ed != "bedrock", Bedrock: ed != "", OwnDomain: own[d]}
		if ed != "" {
			v.Enabled, v.Address = true, d
		}
		if own[d] {
			// Display only: an unreadable list shows no state.
			if p, found, err := rg.ProviderRoute(ctx, d); err == nil && found && p.Bedrock != nil {
				rv.DNSStatus, rv.DNSCheckedAt, rv.DNSFailingSince = p.Bedrock.DNSStatus, p.Bedrock.DNSCheckedAt, p.Bedrock.DNSFailingSince
			}
		}
		v.Routes = append(v.Routes, rv)
	}
	refusal := ""
	if v.Address == "" {
		if len(domains) == 0 {
			refusal = remoteBedrockNoAddress
		} else if regions, err := rg.Regions(ctx); err != nil {
			refusal = remoteBedrockUnreachable
		} else if d, ok := services.VoiceAddressFor(domains, own, regions.Items); ok {
			v.Address = d
		} else {
			// Only addresses in a region that is offline now: the provider's
			// refusal for the first one says why.
			v.Address = domains[0]
		}
	}
	if refusal == "" {
		if p, found, err := rg.ProviderRoute(ctx, v.Address); err != nil {
			refusal = remoteBedrockUnreachable
		} else if found && p.Bedrock != nil {
			refusal = p.Bedrock.Refusal
		}
	}
	v.Available, v.Refusal = refusal == "", refusal
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "bedrock": v})
}

// setRemoteRouteEdition is SetRouteEdition on the managed gateway. Adding
// Bedrock is synchronous against the provider and recorded once it said yes;
// removing it or changing Java is local first, then the outbox, never refused.
func (h *GatewayHandler) setRemoteRouteEdition(w http.ResponseWriter, r *http.Request, rg *services.RemoteGateway, srv *models.Server, domain string, java, bedrock bool) {
	editions, err := rg.ServerEditions(srv.UUID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	prev, ok := editions[domain]
	if !ok {
		httpx.SendJSONError(w, "Route not found for this server", http.StatusNotFound)
		return
	}
	route := bedrockRouteView{Domain: domain, Java: java, Bedrock: bedrock}
	if !bedrock || prev != "" {
		if err := rg.SetEditionWithoutAdding(srv.UUID, domain, java, bedrock); err != nil {
			log.Printf("bedrock: server %d: route %s: %v", srv.ID, domain, err)
			httpx.SendJSONError(w, "Failed to save the route", http.StatusInternalServerError)
			return
		}
		out := map[string]interface{}{"success": true, "route": route}
		if bedrock {
			// Already on: a key not yet in Redis (its publish failed) is
			// handed over now, and the restart it needs is still owed.
			if published, err := h.republishMissingKey(r.Context(), srv); err != nil {
				log.Printf("bedrock: server %d: publish the Floodgate key: %v", srv.ID, err)
				out["restartRequired"], out["warning"] = true, remoteBedrockKeyLate
			} else if published {
				out["restartRequired"] = true
			}
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	if allowed, err := h.bedrockAllowedChecked(r); err != nil {
		bedrockSettingUnreadable(w, err)
		return
	} else if !allowed {
		httpx.SendJSONErrorCode(w, bedrockOff, "bedrock_disabled", http.StatusConflict)
		return
	}
	if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
		return
	}
	keyChanged, err := rg.AddBedrock(r.Context(), srv.ID, srv.UUID, domain, java)
	switch {
	case errors.Is(err, store.ErrRemoteBedrockTaken):
		other := "the other address"
		for d, ed := range editions {
			if ed != "" && d != domain {
				other = d
			}
		}
		httpx.SendJSONErrorCode(w, "With the managed gateway a server has one Bedrock address. Take Bedrock off "+other+" first.", "bedrock_one_address", http.StatusConflict)
		return
	case err != nil:
		if !gate.GatewayProviderError(w, err) {
			log.Printf("bedrock: server %d: route %s: %v", srv.ID, domain, err)
			httpx.SendJSONError(w, "Failed to save the route", http.StatusInternalServerError)
		}
		return
	}
	// The node reads the key at the next start; the reconcile republishes it
	// within a minute should this write fail.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	key, found, err := h.d.Store.GetBedrockKey(srv.ID)
	if err == nil && !found {
		err = errors.New("no stored key after the download")
	}
	if err == nil {
		err = h.bedrockService().PublishKey(ctx, srv.UUID, key)
	}
	if err != nil {
		// Bedrock IS on: an error here would invite a retry that answers
		// without the restart. The reconcile republishes within a minute.
		log.Printf("bedrock: server %d: publish the Floodgate key: %v", srv.ID, err)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "route": route, "restartRequired": true, "warning": remoteBedrockKeyLate})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "route": route, "restartRequired": keyChanged})
}
