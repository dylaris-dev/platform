package services

import (
	"context"
	"fmt"
	"strings"

	"dylaris-core/models"

	"github.com/redis/go-redis/v9"
)

// RouteDeleter is the slice of the gateway provider the remover needs.
// Satisfied structurally by GatewayProvider.
type RouteDeleter interface {
	DeleteRoute(domain string) error
}

// customDomainRouteRemover drops the routes a user holds on a domain they
// failed to prove they own.
type customDomainRouteRemover struct {
	redis   *redis.Client
	gw      RouteDeleter
	servers ServerOwnerLister
}

// ServerOwnerLister is the store slice that maps an account to its servers.
type ServerOwnerLister interface {
	ListServersByOwner(ownerID string) ([]models.Server, error)
}

// NewCustomDomainRouteRemover wires the verifier's route teardown.
func NewCustomDomainRouteRemover(rdb *redis.Client, gw RouteDeleter, servers ServerOwnerLister) RouteRemover {
	return &customDomainRouteRemover{redis: rdb, gw: gw, servers: servers}
}

// OwnedServerUUIDs is the set of servers an account owns, the key that ties a
// managed route back to that account.
func OwnedServerUUIDs(st ServerOwnerLister, userID string) (map[string]bool, error) {
	srvs, err := st.ListServersByOwner(userID)
	if err != nil {
		return nil, fmt.Errorf("list servers of %s: %w", userID, err)
	}
	out := make(map[string]bool, len(srvs))
	for _, s := range srvs {
		out[s.UUID] = true
	}
	return out, nil
}

// RouteHeldBy reports whether an account holds rt.
//
// A route-only entry says so in owner_id. A managed route does not: the hub
// publishes it with no owner at all, so the server it serves is the only way
// back to an account. Reading owner_id alone made every managed route nobody's,
// which is how a tenant's unproven custom domain on a server route outlived its
// deadline and how managed routes never counted against the route allowance.
func RouteHeldBy(rt GatewayRoute, userID string, ownedServers map[string]bool) bool {
	if rt.OwnerID != "" {
		return rt.OwnerID == userID
	}
	if rt.CoreOwned {
		return false
	}
	uuid := rt.ServerUUID
	if uuid == "" {
		uuid = strings.TrimPrefix(rt.TargetIP, "mc_")
		if uuid == rt.TargetIP {
			return false
		}
	}
	return uuid != "" && ownedServers[uuid]
}

// DeleteRoutesForDomain removes this user's routes on exactly that domain.
//
// Exactly, not "and everything under it": every route on a brought domain has
// a claim of its own, so a subdomain is judged by its own claim. Matching by
// suffix let one failed claim on example.com tear down a verified
// play.example.com - or, for a tenant who claimed the parent of a hoster
// domain, their addresses on OUR domain.
//
// Scoped to the OWNER, not to the domain alone: two tenants holding routes under
// one name should not be possible, but if it ever is, one tenant failing a check
// must not tear down the other's routes. Same reasoning as the block being per
// (user, domain) rather than global.
//
// A managed route counts as this user's when it serves one of their servers -
// see RouteHeldBy.
func (r *customDomainRouteRemover) DeleteRoutesForDomain(ctx context.Context, userID, domain string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || userID == "" {
		return nil
	}
	owned, err := OwnedServerUUIDs(r.servers, userID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, rt := range GetRoutesFromRedis(ctx, r.redis) {
		if !RouteHeldBy(rt, userID, owned) {
			continue
		}
		if !strings.EqualFold(rt.Domain, domain) {
			continue
		}
		if derr := r.gw.DeleteRoute(rt.Domain); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}
