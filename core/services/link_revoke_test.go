package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dylaris-core/services/redisacl"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// linkRevokeFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only RevokeWarpAPIKeyByNodeID is overridden.
type linkRevokeFakeStore struct {
	store.Store
	revokeErr   map[string]error
	revokeCalls []string
	rows        []store.CoreLinkRoute
	listErr     error
}

func (f *linkRevokeFakeStore) RevokeWarpAPIKeyByNodeID(nodeID string) error {
	f.revokeCalls = append(f.revokeCalls, nodeID)
	return f.revokeErr[nodeID]
}

func (f *linkRevokeFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return f.rows, f.listErr
}

// linkRevokeFakeGateway embeds GatewayProvider (nil) so it satisfies the
// full interface at compile time; only LinkToken and DeleteCoreOwnedRoute
// (the two methods RevokeLinkKitTeardown calls) are overridden.
type linkRevokeFakeGateway struct {
	GatewayProvider
	tunnelToken    string
	deleteErrFor   map[string]error
	deletedDomains []string
	// tokenedFor records which identities the KIT teardown was run for. The
	// revoke alone no longer tells them apart: the account teardown revokes every
	// key of the owner, and only a link kit gets the ACL/tunnel/route treatment.
	tokenedFor []string
}

func (g *linkRevokeFakeGateway) LinkToken(nodeID string) string {
	g.tokenedFor = append(g.tokenedFor, nodeID)
	return g.tunnelToken
}

func (g *linkRevokeFakeGateway) DeleteCoreOwnedRoute(domain string) error {
	g.deletedDomains = append(g.deletedDomains, domain)
	return g.deleteErrFor[domain]
}

func TestRevokeLinkKitTeardown_RevokeFails_NoSideEffects(t *testing.T) {
	rdb := newQueueTestRedis(t)
	fs := &linkRevokeFakeStore{revokeErr: map[string]error{"link-1": errors.New("db down")}}
	gw := &linkRevokeFakeGateway{tunnelToken: "tunnel-1"}
	prov := redisacl.NewProvisioner(rdb)

	removed, err := RevokeLinkKitTeardown(context.Background(), fs, gw, rdb, prov, "link-1", "owner-1")

	if err == nil {
		t.Fatal("expected error when the durable revoke fails")
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if len(gw.deletedDomains) != 0 {
		t.Errorf("expected no route deletion attempts after a failed revoke, got %v", gw.deletedDomains)
	}
}

func TestRevokeLinkKitTeardown_RemovesOnlyMatchingCoreOwnedRoutes(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &linkRevokeFakeStore{revokeErr: map[string]error{}}
	const tunnelToken = "tunnel-1"
	gw := &linkRevokeFakeGateway{tunnelToken: tunnelToken}
	prov := redisacl.NewProvisioner(rdb)

	// The stored rows are what a revocation works from now. A hub-managed route
	// cannot appear here at all - only route-only entries are ever recorded -
	// so the "not core-owned" case this used to seed into Redis is now
	// unrepresentable rather than merely filtered out.
	fs.rows = []store.CoreLinkRoute{
		{Domain: "a.example.com", OwnerID: "owner-1", LinkToken: tunnelToken},
		{Domain: "b.example.com", OwnerID: "owner-2", LinkToken: tunnelToken},    // different owner
		{Domain: "d.example.com", OwnerID: "owner-1", LinkToken: "other-tunnel"}, // different tunnel
		{Domain: "e.example.com", OwnerID: "owner-1", LinkToken: tunnelToken},
	}
	// Redis holds them too, and deliberately holds one MORE than the rows do:
	// the revocation must not depend on the cache in either direction.
	seedGatewayRoute(t, rdb, "a.example.com", GatewayRoute{CoreOwned: true, OwnerID: "owner-1", TunnelID: tunnelToken})
	seedGatewayRoute(t, rdb, "c.example.com", GatewayRoute{CoreOwned: false, OwnerID: "owner-1", TunnelID: tunnelToken})

	removed, err := RevokeLinkKitTeardown(ctx, fs, gw, rdb, prov, "link-1", "owner-1")
	if err != nil {
		t.Fatalf("RevokeLinkKitTeardown: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	deleted := map[string]bool{}
	for _, d := range gw.deletedDomains {
		deleted[d] = true
	}
	if !deleted["a.example.com"] || !deleted["e.example.com"] {
		t.Errorf("deletedDomains = %v, want a.example.com and e.example.com", gw.deletedDomains)
	}
	if deleted["b.example.com"] || deleted["c.example.com"] || deleted["d.example.com"] {
		t.Errorf("deletedDomains = %v, want only owner-matched core-owned same-tunnel routes", gw.deletedDomains)
	}

	if len(fs.revokeCalls) != 1 || fs.revokeCalls[0] != "link-1" {
		t.Errorf("revokeCalls = %v, want [link-1]", fs.revokeCalls)
	}
}

func TestRevokeLinkKitTeardown_RouteDeleteError_ContinuesAndSkipsCount(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &linkRevokeFakeStore{revokeErr: map[string]error{}}
	const tunnelToken = "tunnel-2"
	gw := &linkRevokeFakeGateway{
		tunnelToken:  tunnelToken,
		deleteErrFor: map[string]error{"f.example.com": errors.New("redis blip")},
	}
	prov := redisacl.NewProvisioner(rdb)

	fs.rows = []store.CoreLinkRoute{
		{Domain: "f.example.com", OwnerID: "owner-1", LinkToken: tunnelToken},
		{Domain: "g.example.com", OwnerID: "owner-1", LinkToken: tunnelToken},
	}

	removed, err := RevokeLinkKitTeardown(ctx, fs, gw, rdb, prov, "link-2", "owner-1")
	if err != nil {
		t.Fatalf("RevokeLinkKitTeardown: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (the failed delete must not be counted)", removed)
	}
}

func TestRevokeLinkKitTeardown_NoMatchingRoutes_ReturnsZero(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &linkRevokeFakeStore{revokeErr: map[string]error{}}
	gw := &linkRevokeFakeGateway{tunnelToken: "tunnel-3"}
	prov := redisacl.NewProvisioner(rdb)

	removed, err := RevokeLinkKitTeardown(ctx, fs, gw, rdb, prov, "link-3", "owner-1")
	if err != nil {
		t.Fatalf("RevokeLinkKitTeardown: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestRevokeLinkKitTeardown_DeletesTunnelKey(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &linkRevokeFakeStore{revokeErr: map[string]error{}}
	const tunnelToken = "tunnel-4"
	gw := &linkRevokeFakeGateway{tunnelToken: tunnelToken}
	prov := redisacl.NewProvisioner(rdb)

	if err := rdb.Set(ctx, "link:"+tunnelToken, "some-value", 0).Err(); err != nil {
		t.Fatalf("seed tunnel key: %v", err)
	}

	if _, err := RevokeLinkKitTeardown(ctx, fs, gw, rdb, prov, "link-4", "owner-1"); err != nil {
		t.Fatalf("RevokeLinkKitTeardown: %v", err)
	}

	n, err := rdb.Exists(ctx, "link:"+tunnelToken).Result()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if n != 0 {
		t.Errorf("tunnel key still exists after teardown")
	}
}

// seedGatewayRoute writes a route in the same format GetRoutesFromRedis reads:
// route:{domain} JSON + membership in sys:index:routes.
func seedGatewayRoute(t *testing.T, rdb *redis.Client, domain string, route GatewayRoute) {
	t.Helper()
	route.Domain = domain
	data, err := json.Marshal(route)
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}
	ctx := context.Background()
	if err := rdb.Set(ctx, "route:"+domain, data, 0).Err(); err != nil {
		t.Fatalf("seed route:%s: %v", domain, err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", domain).Err(); err != nil {
		t.Fatalf("seed sys:index:routes: %v", err)
	}
}

// The regression the durable rows exist for. A revocation used to enumerate the
// LIVE routing table, so its completeness depended on the cache being complete:
// a Redis that had lost the entries reported nothing to remove and left the
// routes stored - and with a republisher running, they would come back a minute
// later pointing at a link that had just been torn down.
func TestRevokeLinkKitTeardown_RemovesRoutesMissingFromRedis(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	const tunnelToken = "tunnel-9"
	fs := &linkRevokeFakeStore{revokeErr: map[string]error{}, rows: []store.CoreLinkRoute{
		{Domain: "gone.example.com", OwnerID: "owner-1", LinkToken: tunnelToken},
	}}
	gw := &linkRevokeFakeGateway{tunnelToken: tunnelToken}
	prov := redisacl.NewProvisioner(rdb)
	// Nothing seeded into Redis at all: the cache is exactly as empty as it is
	// after the restart that started this.

	removed, err := RevokeLinkKitTeardown(ctx, fs, gw, rdb, prov, "link-9", "owner-1")
	if err != nil {
		t.Fatalf("RevokeLinkKitTeardown: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 - the route is stored, so it must be removed whatever the cache holds", removed)
	}
	if len(gw.deletedDomains) != 1 || gw.deletedDomains[0] != "gone.example.com" {
		t.Errorf("deletedDomains = %v, want [gone.example.com]", gw.deletedDomains)
	}
}

// A failed listing must not read as "this link has no routes". Reporting a
// clean teardown while the routes are still stored is the one outcome that
// cannot be retried, because nothing left says there is anything to retry.
func TestRevokeLinkKitTeardown_ListFails_RemovesNothing(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &linkRevokeFakeStore{revokeErr: map[string]error{}, listErr: errors.New("db down")}
	gw := &linkRevokeFakeGateway{tunnelToken: "tunnel-10"}
	prov := redisacl.NewProvisioner(rdb)

	removed, _ := RevokeLinkKitTeardown(ctx, fs, gw, rdb, prov, "link-10", "owner-1")
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if len(gw.deletedDomains) != 0 {
		t.Errorf("deletedDomains = %v, want none attempted on a failed listing", gw.deletedDomains)
	}
}
