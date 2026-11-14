package gateway

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Read-only helpers for the customer side of the managed gateway: who the key
// is, and whether an address is free. Native rather than adapters, like the
// beam handlers: no panel handler answers either question for a key.

// externalLimits uses the platform limit convention: null = no cap, 0 = none.
type externalLimits struct {
	MaxLinks         *int64 `json:"maxLinks"`
	MaxRoutes        *int64 `json:"maxRoutes"`
	MaxCustomDomains *int64 `json:"maxCustomDomains"`
}

type externalAccount struct {
	UserID string `json:"userId"`
	// Product is managed, route_only or none.
	Product string `json:"product"`
	Managed bool   `json:"managed"`
	// EntitlementSource is where the entitlement comes from: plan, grant,
	// plan+grant, unlimited (self-host or admin), suspended or none. Not
	// "state": /usage already emits an entitlementState object.
	EntitlementSource string         `json:"entitlementSource"`
	Suspended         bool           `json:"suspended"`
	Scopes            []string       `json:"scopes"`
	Limits            externalLimits `json:"limits"`
}

// GetAccount GET /api/external/gateway/account - who the key belongs to: product (managed, route_only, none), entitlement source, suspension, the key's scopes and the gateway caps. Stays reachable while suspended so a client can say why.
func (h *ExternalGatewayHandler) GetAccount(w http.ResponseWriter, r *http.Request) {
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	d := h.gw.d
	fail := func(what string, err error) {
		log.Printf("external account %s: %s: %v", owner, what, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
	}
	// isAdmin=false on purpose: a key never carries operator powers here.
	ent, err := services.EffectiveEntitlement(d.Store, owner, time.Now(), d.StoreEnabled, false)
	if err != nil {
		fail("entitlement", err)
		return
	}
	product, managed, err := accountProduct(d.Store, owner, ent)
	if err != nil {
		fail("managed lookup", err)
		return
	}
	lim, err := services.EffectiveLimits(d.Store, owner)
	if err != nil {
		fail("limits", err)
		return
	}
	customCap, _ := customDomainCapFor(d.Store, owner)
	scopes := []string{}
	if k := reqctx.APIKeyFromContext(r); k != nil {
		scopes = append(scopes, k.Scope.Permissions...)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "account": externalAccount{
		UserID:            owner,
		Product:           product,
		Managed:           managed,
		EntitlementSource: ent.Source,
		Suspended:         ent.Source == services.EntitlementSourceSuspended || guard.OwnerRefusal(d.Store, d.SuspendGrace, r, owner) != "",
		Scopes:            scopes,
		Limits:            externalLimits{MaxLinks: lim.MaxLinks, MaxRoutes: h.gw.effectiveRouteLimit(owner), MaxCustomDomains: customCap},
	}})
}

// accountProduct is the gateway product an account holds: managed (the
// managed plan's traffic row) over route_only (the entitlement), else none.
// Shared by the external account answer and the admin view of one customer.
func accountProduct(st store.Store, userID string, ent services.Entitlement) (product string, managed bool, err error) {
	managed, err = services.HasManagedProduct(st, userID)
	if err != nil {
		return "", false, err
	}
	switch {
	case managed:
		return "managed", true, nil
	case ent.RouteOnly:
		return "route_only", false, nil
	}
	return "none", false, nil
}

// domainRefusalReason files resolveRouteDomain's refusal under the public
// reason codes. By message fragment, like externalErrorCodes: a reworded
// message falls back to invalid, and the tests pin the three that matter.
func domainRefusalReason(err error) string {
	m := err.Error()
	switch {
	case strings.Contains(m, "reserved"):
		return "blocked"
	case strings.Contains(m, "not configured"), strings.Contains(m, "not enabled"):
		return "not_hosted"
	}
	return "invalid"
}

// CheckDomain GET /api/external/gateway/check-domain?domain=<fqdn> - whether the caller could create a route on that domain: available, and a reason (taken, blocked, invalid, not_hosted) when not. A domain the caller already holds is available; nothing is said about who holds a taken one.
func (h *ExternalGatewayHandler) CheckDomain(w http.ResponseWriter, r *http.Request) {
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	in := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	answer := func(domain string, available bool, reason string) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": domain, "available": available, "reason": reason})
	}
	req := struct {
		Domain       string `json:"domain"`
		Subdomain    string `json:"subdomain"`
		HosterDomain string `json:"hosterDomain"`
		CustomDomain string `json:"customDomain"`
		TargetPort   int    `json:"targetPort"`
	}{Domain: in}
	// isAdmin=false: the blocked prefixes apply to a key, whoever owns it.
	final, _, err := h.gw.resolveRouteDomain(&req, false)
	if err != nil {
		answer(in, false, domainRefusalReason(err))
		return
	}
	d := h.gw.d
	// The stored row first, as route create asks it: it outranks Redis and
	// is the only place a suspended route still shows. Ownership before
	// suspension, so another owner learns only "taken".
	stored, err := d.Store.GetCoreLinkRoute(final)
	if err != nil {
		log.Printf("external check-domain %s: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	switch {
	case stored != nil && stored.OwnerID != owner:
		answer(final, false, "taken")
		return
	case stored != nil && stored.SuspendedAt != nil:
		answer(final, false, "blocked")
		return
	}
	val, err := d.Redis.Get(h.gw.ctx(), "route:"+final).Result()
	if errors.Is(err, redis.Nil) { // no route: free
		answer(final, true, "")
		return
	}
	if err != nil {
		log.Printf("external check-domain %s: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	var rt services.GatewayRoute
	if json.Unmarshal([]byte(val), &rt) == nil {
		owned, oerr := services.OwnedServerUUIDs(d.Store, owner)
		if oerr != nil {
			log.Printf("external check-domain %s: %v", owner, oerr)
			writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
			return
		}
		if services.RouteHeldBy(rt, owner, owned) {
			answer(final, true, "")
			return
		}
	}
	answer(final, false, "taken")
}
