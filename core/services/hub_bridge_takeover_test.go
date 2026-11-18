package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dylaris-core/models"
)

// readRoute decodes route:{domain} the way the edge and the panel both read it.
func readRoute(t *testing.T, g *RedisGateway, domain string) GatewayRoute {
	t.Helper()
	val, err := g.redis.Get(context.Background(), "route:"+domain).Result()
	if err != nil {
		t.Fatalf("get route:%s: %v", domain, err)
	}
	var r GatewayRoute
	if err := json.Unmarshal([]byte(val), &r); err != nil {
		t.Fatalf("unmarshal route:%s: %v", domain, err)
	}
	return r
}

// A route-only entry is published straight to Redis with no Postgres row behind
// it, so nothing else in the system can enforce that one domain has one owner.
// The managed path gets that for free: it goes through the hub queue, and the
// hub's routes table has a unique index on domain and answers a second create
// with ErrRouteExists.
//
// Without the check here, the second tenant's SET simply overwrites the first
// tenant's route - tunnel_id and owner_id and all - so their players are handed
// to a stranger's Link and the domain disappears from their own panel listing
// (ListLinkRoutes filters on owner_id).
func TestCreateRouteViaLink_RefusesADomainAnotherTenantHolds(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)

	if err := g.CreateRouteViaLink("victim", "play.example.com", "victim-tok", "192.168.1.50", 25565); err != nil {
		t.Fatalf("victim create: %v", err)
	}
	err := g.CreateRouteViaLink("attacker", "play.example.com", "attacker-tok", "10.0.0.9", 25565)
	if err == nil {
		t.Error("a second tenant claimed a domain that was already taken, and got no error")
	}

	got := readRoute(t, g, "play.example.com")
	if got.OwnerID != "victim" || got.TunnelID != "victim-tok" {
		t.Errorf("route now points at owner=%q tunnel=%q; the first tenant's route was overwritten",
			got.OwnerID, got.TunnelID)
	}
}

// A managed server's route is written by the hub from its own table, so it
// carries no core_owned flag. A tenant must not be able to shadow it either -
// the overwrite would hold until the hub's next reconcile pass rewrites it,
// which is a five-minute outage that repeats every time the attacker re-posts.
func TestCreateRouteViaLink_RefusesAManagedServersDomain(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)

	managed, err := json.Marshal(map[string]interface{}{
		"tunnel_id": "node-link-tok", "target_ip": "mc_srv-uuid", "target_port": 25565,
		"server_uuid": "srv-uuid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), "route:mc.example.com", managed, 0).Err(); err != nil {
		t.Fatal(err)
	}

	if err := g.CreateRouteViaLink("attacker", "mc.example.com", "attacker-tok", "10.0.0.9", 25565); err == nil {
		t.Error("a tenant claimed a managed server's domain, and got no error")
	}
	if got := readRoute(t, g, "mc.example.com"); got.TunnelID != "node-link-tok" {
		t.Errorf("tunnel_id = %q, want node-link-tok; the managed route was overwritten", got.TunnelID)
	}
}

// The same collision from the other side. A managed server route has no row in
// the hub's table for a route-only domain, so the hub's unique index sees
// nothing to reject and its next sync writes route:{domain} over the tenant's
// entry - a takeover with a five-minute delay instead of an instant one.
func TestCreateServerRoute_RefusesATenantsRouteOnlyDomain(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1"}
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1}

	if err := g.CreateRouteViaLink("victim", "play.example.com", "victim-tok", "192.168.1.50", 25565); err != nil {
		t.Fatalf("victim create: %v", err)
	}
	if err := g.CreateServerRoute(10, "attacker", "play.example.com", 25565); err == nil {
		t.Error("a managed server claimed a tenant's route-only domain, and got no error")
	}

	n, err := rdb.LLen(context.Background(), hubQueueKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d queue messages pushed; the create should not have reached the hub", n)
	}
	if got := readRoute(t, g, "play.example.com"); got.OwnerID != "victim" {
		t.Errorf("owner_id = %q, want victim", got.OwnerID)
	}
}

// The stored row is the claim, not the Redis key. When Redis loses the key
// (restart without persistence) a managed create in the window before the
// republisher's next tick must still be refused, or the hub's sync takes the
// domain for good.
func TestCreateServerRoute_RefusesARouteOnlyDomainWhoseKeyIsGone(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1"}
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1}

	if err := g.CreateRouteViaLink("victim", "play.example.com", "victim-tok", "192.168.1.50", 25565); err != nil {
		t.Fatalf("victim create: %v", err)
	}
	if err := rdb.Del(context.Background(), "route:play.example.com").Err(); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateServerRoute(10, "attacker", "play.example.com", 25565); !errors.Is(err, ErrRouteDomainTaken) {
		t.Fatalf("create = %v, want ErrRouteDomainTaken", err)
	}
	if n, _ := rdb.LLen(context.Background(), hubQueueKey).Result(); n != 0 {
		t.Errorf("%d queue messages pushed; the create should not have reached the hub", n)
	}
}

// Managed-vs-managed must NOT be refused here. The hub's unique index already
// keeps the first route, and a deleted managed route's Redis key outlives the
// delete (the zombie sweep runs only on the leader, up to five minutes later) -
// so a check on this path would reject the legitimate delete-then-recreate.
func TestCreateServerRoute_StillQueuesOverAManagedRoutesKey(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1"}
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1}

	managed, err := json.Marshal(map[string]interface{}{
		"tunnel_id": "old-tok", "target_ip": "mc_old-uuid", "target_port": 25565,
		"server_uuid": "old-uuid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), "route:mc.example.com", managed, 0).Err(); err != nil {
		t.Fatal(err)
	}

	if err := g.CreateServerRoute(10, "owner-1", "mc.example.com", 25565); err != nil {
		t.Fatalf("CreateServerRoute: %v", err)
	}
	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 1 {
		t.Fatalf("got %d queue messages, want 1", len(msgs))
	}
}

// Re-creating your OWN route has to keep working: it is how a tenant changes the
// target host or port of a route they already hold.
func TestCreateRouteViaLink_OwnerMayRewriteTheirOwnRoute(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)

	if err := g.CreateRouteViaLink("owner-2", "play.example.com", "tok", "192.168.1.50", 25565); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := g.CreateRouteViaLink("owner-2", "play.example.com", "tok", "192.168.1.77", 25566); err != nil {
		t.Fatalf("owner re-create of their own route: %v", err)
	}

	got := readRoute(t, g, "play.example.com")
	if got.TargetIP != "192.168.1.77" || got.TargetPort != 25566 {
		t.Errorf("route = %+v, want the owner's new target", got)
	}
	// And the stored row follows the edit. Leaving it on the old target would
	// have the republisher undo the change on its next pass.
	if row := fs.routes["play.example.com"]; row.TargetHost != "192.168.1.77" || row.TargetPort != 25566 {
		t.Errorf("stored row = %+v, want the owner's new target", row)
	}
}

// The route has to survive Redis. Creating one records it, so the republisher
// has something to put back after a restart that emptied the cache.
func TestCreateRouteViaLink_RecordsTheRouteDurably(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)

	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok-1", "192.168.1.50", 25566); err != nil {
		t.Fatalf("CreateRouteViaLink: %v", err)
	}

	row, ok := fs.routes["play.example.com"]
	if !ok {
		t.Fatal("no stored row: the route exists only in Redis, which is the defect this replaced")
	}
	if row.OwnerID != "owner-1" || row.LinkToken != "tok-1" || row.TargetHost != "192.168.1.50" || row.TargetPort != 25566 {
		t.Errorf("stored row = %+v, does not match what was published", row)
	}
}

// SETNX was a sufficient arbiter of "who owns this domain" only while Redis
// held the only copy. With a row behind it, an emptied cache would let a second
// tenant claim a domain the first still owns - and the republisher would then
// fight the live entry every minute. The row answers when the cache cannot,
// which is exactly the moment the question matters.
func TestCreateRouteViaLink_StoredOwnerOutranksAnEmptyCache(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)

	if err := g.CreateRouteViaLink("victim", "play.example.com", "victim-tok", "127.0.0.1", 25565); err != nil {
		t.Fatalf("victim create: %v", err)
	}
	// Redis loses it, exactly as a restart without persistence would.
	if err := rdb.Del(context.Background(), "route:play.example.com").Err(); err != nil {
		t.Fatalf("del: %v", err)
	}

	err := g.CreateRouteViaLink("attacker", "play.example.com", "attacker-tok", "10.0.0.9", 25565)
	if !errors.Is(err, ErrRouteDomainTaken) {
		t.Fatalf("second tenant's create = %v, want ErrRouteDomainTaken", err)
	}
	if row := fs.routes["play.example.com"]; row.OwnerID != "victim" {
		t.Errorf("stored owner = %q, want victim", row.OwnerID)
	}
	if n, _ := rdb.Exists(context.Background(), "route:play.example.com").Result(); n != 0 {
		t.Error("the refused create published a route anyway")
	}
}

// If the row cannot be written, the claim is given back. A live entry with no
// row behind it would be listed by the panel, was reported to the tenant as a
// failure, and - now that every removal path works from the rows - could never
// be taken away again.
func TestCreateRouteViaLink_RollsBackTheClaimWhenTheRowFails(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.upsertErr = errors.New("db down")

	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok-1", "127.0.0.1", 25565); err == nil {
		t.Fatal("expected an error when the durable write fails")
	}
	if n, _ := rdb.Exists(context.Background(), "route:play.example.com").Result(); n != 0 {
		t.Error("the claim was kept after the durable write failed")
	}
}

// Deleting drops the row FIRST. The other order leaves a window in which the
// republisher restores the route being deleted, and a crash inside that window
// brings it back for good.
func TestDeleteCoreOwnedRoute_RemovesTheRowToo(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok-1", "127.0.0.1", 25565); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := g.DeleteCoreOwnedRoute("play.example.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, ok := fs.routes["play.example.com"]; ok {
		t.Error("the row survived the delete, so the republisher would bring the route back")
	}
	if n, _ := rdb.Exists(context.Background(), "route:play.example.com").Result(); n != 0 {
		t.Error("the live entry survived the delete")
	}
}
