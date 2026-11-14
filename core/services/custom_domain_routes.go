package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// RouteDeleter is the slice of the gateway provider the remover needs.
// Satisfied structurally by GatewayProvider.
type RouteDeleter interface {
	DeleteRoute(domain string) error
}

// RemoteRouteReader is the store slice the remote branch reads the route row
// from.
type RemoteRouteReader interface {
	GetRemoteRoute(domain string) (*store.RemoteRoute, error)
}

// ProviderClaimReleaser gives the provider's claim on a domain back once no
// other local claim needs it. Satisfied by RemoteGateway.
type ProviderClaimReleaser interface {
	ReleaseProviderClaim(domain, exceptUserID string) error
}

// customDomainRouteRemover drops the routes a user holds on a domain they
// failed to prove they own.
type customDomainRouteRemover struct {
	redis         *redis.Client
	gw            RouteDeleter
	servers       ServerOwnerLister
	remote        RemoteRouteReader
	claims        ProviderClaimReleaser
	gatewayRemote func() (remote, known bool)
}

// ServerOwnerLister is the store slice that maps an account to its servers.
type ServerOwnerLister interface {
	ListServersByOwner(ownerID string) ([]models.Server, error)
}

// NewCustomDomainRouteRemover wires the verifier's route teardown. A nil
// gatewayRemote is local mode.
func NewCustomDomainRouteRemover(rdb *redis.Client, gw RouteDeleter, servers ServerOwnerLister,
	remote RemoteRouteReader, claims ProviderClaimReleaser, gatewayRemote func() (remote, known bool)) RouteRemover {
	return &customDomainRouteRemover{redis: rdb, gw: gw, servers: servers, remote: remote, claims: claims, gatewayRemote: gatewayRemote}
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
	switch err := LocalGatewayErr(r.gatewayRemote); {
	case errors.Is(err, ErrGatewayRemote):
		return r.deleteRemote(userID, domain)
	case err != nil:
		// Not knowing where the routes are is not "there are none": the
		// verifier then leaves the claim for the next pass.
		return err
	}
	owned, err := OwnedServerUUIDs(r.servers, userID)
	if err != nil {
		return err
	}
	// A Redis that could not be read is not "no routes": the caller then marks
	// the claim done and nothing looks at those routes again.
	if err := r.redis.SCard(ctx, "sys:index:routes").Err(); err != nil {
		return fmt.Errorf("read routes: %w", err)
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

// deleteRemote is the remote-mode teardown: the remote_routes row of exactly
// that domain, when it serves one of the user's servers, is deleted through the
// provider (gw is the switching gateway, so this queues the outbox delete).
// Then the provider's claim is released unless another user's claim still
// needs it; queued after the route delete, it reaches the provider second.
func (r *customDomainRouteRemover) deleteRemote(userID, domain string) error {
	row, err := r.remote.GetRemoteRoute(domain)
	if err != nil {
		return fmt.Errorf("read route %s: %w", domain, err)
	}
	if row != nil {
		owned, err := OwnedServerUUIDs(r.servers, userID)
		if err != nil {
			return err
		}
		if owned[row.ServerUUID] {
			if err := r.gw.DeleteRoute(row.Domain); err != nil {
				return err
			}
		}
	}
	return r.claims.ReleaseProviderClaim(domain, userID)
}
