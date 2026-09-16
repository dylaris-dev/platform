package services

import (
	"context"
	"testing"

	"dylaris-core/store"
)

// What a delete has to actually remove, and why the queue alone was not it.
//
// A route-only address is CORE's: Core publishes it to Redis itself and records
// it in core_link_routes. The hub has no row for it. DeleteRoute used to only
// push "delete_route" onto the hub queue, so for these addresses it deleted
// nothing at all - and RepublishCoreOwnedRoutes wrote the stored row straight
// back on its next tick. Deleting from the admin Routes screen reported success
// and the address kept routing, which is what was reported from production.
func TestDeleteRouteRemovesTheDurableRow(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	ctx := context.Background()

	const domain = "play.example.com"
	fs.routes[domain] = store.CoreLinkRoute{Domain: domain, OwnerID: "tenant-1"}
	if err := rdb.Set(ctx, "route:"+domain, "{}", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", domain).Err(); err != nil {
		t.Fatal(err)
	}

	if err := g.DeleteRoute(domain); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}

	// The row is the one that matters: it is what the republisher reads.
	if _, still := fs.routes[domain]; still {
		t.Error("the durable row survived, so the republisher puts this address back within a minute")
	}
	if n, _ := rdb.Exists(ctx, "route:"+domain).Result(); n != 0 {
		t.Error("the live routing entry survived")
	}
	if member, _ := rdb.SIsMember(ctx, "sys:index:routes", domain).Result(); member {
		t.Error("the route index still lists the domain")
	}
}

// The hub still has to hear about it. A route the HUB owns has no row here, and
// the queue message is the only thing that reaches its database - so the fix
// must not have traded one half of the delete for the other.
func TestDeleteRouteStillTellsTheHub(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	ctx := context.Background()

	if err := g.DeleteRoute("hub-owned.example.com"); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}

	n, err := rdb.LLen(ctx, hubQueueKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("hub queue holds %d messages, want 1 - a hub-owned route is deleted by nothing else", n)
	}
}

// Both halves run for a Core-owned route, not one or the other. The queue
// message is harmless for a domain the hub does not know (it deletes zero rows
// without erroring), and leaving it out would mean a route that exists on BOTH
// sides only gets cleaned up on one.
func TestDeleteRouteDoesBothHalves(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	ctx := context.Background()

	const domain = "both.example.com"
	fs.routes[domain] = store.CoreLinkRoute{Domain: domain, OwnerID: "tenant-1"}

	if err := g.DeleteRoute(domain); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}

	if _, still := fs.routes[domain]; still {
		t.Error("the durable row survived")
	}
	if n, _ := rdb.LLen(ctx, hubQueueKey).Result(); n != 1 {
		t.Errorf("hub queue holds %d messages, want 1", n)
	}
}

// A store that cannot answer must not be reported as a successful delete. The
// caller would tell the operator the address is gone while it keeps routing,
// which is the exact failure this whole test file exists for.
func TestDeleteRouteFailsLoudlyWhenTheStoreCannotAnswer(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	fs.routeErr = hubBridgeErr("database is down")

	if err := g.DeleteRoute("play.example.com"); err == nil {
		t.Error("DeleteRoute reported success while it could not tell whether a row existed")
	}
}

// The cache entry of a route the HUB owns. Nothing else clears it.
//
// The queue message removes the hub's DB ROW; the hub's sync only ever WRITES
// keys, and the one thing that clears an orphan is a leader-only sweep. So a
// route deleted from the admin screen kept its `route:` key and kept answering
// at the edge. Found on production weeks after the delete, on an address whose
// node had been removed.
func TestDeleteRouteDropsTheCacheEntryOfAHubOwnedRoute(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	ctx := context.Background()

	const domain = "hub-owned.example.com"
	if err := rdb.Set(ctx, "route:"+domain, "{}", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", domain).Err(); err != nil {
		t.Fatal(err)
	}

	if err := g.DeleteRoute(domain); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}

	if n, _ := rdb.Exists(ctx, "route:"+domain).Result(); n != 0 {
		t.Error("the live routing entry survived, so the address keeps answering at the edge until a leader sweep happens to notice")
	}
	if member, _ := rdb.SIsMember(ctx, "sys:index:routes", domain).Result(); member {
		t.Error("the route index still lists the domain")
	}
	// Still durable: the cache drop is on top of the queue message, never
	// instead of it. Without the message the hub's next sync writes the key back.
	if n, _ := rdb.LLen(ctx, hubQueueKey).Result(); n != 1 {
		t.Errorf("hub queue holds %d messages, want 1", n)
	}
}
