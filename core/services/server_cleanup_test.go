package services

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/redis/go-redis/v9"
)

// routeDeleteGateway records what was asked of the hub. Embedding the interface
// makes anything else this might call panic rather than pass quietly.
type routeDeleteGateway struct {
	GatewayProvider
	deleted       []string
	serverDeletes []string
}

func (g *routeDeleteGateway) DeleteServerRoutes(uuid string) error {
	g.serverDeletes = append(g.serverDeletes, uuid)
	return nil
}

func (g *routeDeleteGateway) DeleteRoute(domain string) error {
	g.deleted = append(g.deleted, domain)
	return nil
}

// seedRoute writes one route exactly as the platform stores it: the entry plus
// its membership in the index the readers enumerate.
func seedRoute(t *testing.T, rdb *redis.Client, r GatewayRoute) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}
	ctx := context.Background()
	if err := rdb.Set(ctx, "route:"+r.Domain, b, 0).Err(); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", r.Domain).Err(); err != nil {
		t.Fatalf("seed index: %v", err)
	}
}

func routeExists(t *testing.T, rdb *redis.Client, domain string) bool {
	t.Helper()
	n, err := rdb.Exists(context.Background(), "route:"+domain).Result()
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	return n > 0
}

// Both shapes of route we have ever written have to be caught. Routes from
// before server_uuid was persisted carry only target_ip = "mc_<uuid>"; newer
// ones carry both. Missing either shape leaves the address answering, and in
// gateway routing that address is the only way into the server.
func TestRemoveDeletedServers_MatchesBothShapesAndLeavesOthersAlone(t *testing.T) {
	rdb := newQueueTestRedis(t)
	seedRoute(t, rdb, GatewayRoute{Domain: "new.example.com", ServerUUID: "uuid-a", TargetIP: "mc_uuid-a"})
	seedRoute(t, rdb, GatewayRoute{Domain: "old.example.com", TargetIP: "mc_uuid-b"})
	seedRoute(t, rdb, GatewayRoute{Domain: "keep.example.com", ServerUUID: "uuid-c", TargetIP: "mc_uuid-c"})

	gw := &routeDeleteGateway{}
	got := RemoveDeletedServers(context.Background(), gw, rdb, []string{"uuid-a", "uuid-b"})

	if got != 2 {
		t.Errorf("matched %d route(s), want 2", got)
	}
	for _, d := range []string{"new.example.com", "old.example.com"} {
		if routeExists(t, rdb, d) {
			t.Errorf("%s still answers after its server was deleted", d)
		}
	}
	if !routeExists(t, rdb, "keep.example.com") {
		t.Fatal("removed a route belonging to a server that was not deleted")
	}
	// The index too: a domain left in sys:index:routes is what the republisher
	// and the hub sweep enumerate, so a half-removed route comes back.
	members, err := rdb.SMembers(context.Background(), "sys:index:routes").Result()
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(members) != 1 || members[0] != "keep.example.com" {
		t.Errorf("index = %v, want only keep.example.com", members)
	}
}

// The hub owns the durable copy. Dropping the Redis entry without telling it
// means its next sync republishes the route from a row it still has.
func TestRemoveDeletedServers_TellsTheHubAsWellAsDroppingTheKey(t *testing.T) {
	rdb := newQueueTestRedis(t)
	seedRoute(t, rdb, GatewayRoute{Domain: "a.example.com", ServerUUID: "uuid-a"})

	gw := &routeDeleteGateway{}
	RemoveDeletedServers(context.Background(), gw, rdb, []string{"uuid-a"})

	if len(gw.deleted) != 1 || gw.deleted[0] != "a.example.com" {
		t.Fatalf("hub was told about %v, want [a.example.com] - otherwise its next sync writes the route back", gw.deleted)
	}
}

// Without a gateway there is no hub to tell, and the cache drop is then the only
// cleanup there is. It must still happen. Not a production shape - Core always
// builds a gateway - but it is the shape every caller reaches through a nil
// field, and the route is just as dead there.
func TestRemoveDeletedServers_StillClearsTheCacheWithNoGateway(t *testing.T) {
	rdb := newQueueTestRedis(t)
	seedRoute(t, rdb, GatewayRoute{Domain: "a.example.com", ServerUUID: "uuid-a"})

	if got := RemoveDeletedServers(context.Background(), nil, rdb, []string{"uuid-a"}); got != 1 {
		t.Fatalf("matched %d, want 1", got)
	}
	if routeExists(t, rdb, "a.example.com") {
		t.Error("route survived a delete on an install with no hub")
	}
}

// A node with no servers, and an empty uuid, must not match a route. An empty
// string reaching the matcher would otherwise hit every route whose server_uuid
// was never set - which is every managed route from before that column.
func TestRemoveDeletedServers_IgnoresAnEmptyList(t *testing.T) {
	rdb := newQueueTestRedis(t)
	seedRoute(t, rdb, GatewayRoute{Domain: "a.example.com", TargetIP: "mc_uuid-a"})

	for _, uuids := range [][]string{nil, {}, {""}} {
		if got := RemoveDeletedServers(context.Background(), nil, rdb, uuids); got != 0 {
			t.Errorf("uuids=%v matched %d route(s), want 0", uuids, got)
		}
	}
	if !routeExists(t, rdb, "a.example.com") {
		t.Fatal("an empty uuid matched a route that had none")
	}
}

// The per-server Redis keys, which lived in the server handler and were the half
// the first version of this fix forgot. Most carry a TTL, but the log stream and
// the stats buffer are STREAMS with no expiry and no other remover, so every
// deleted server used to leak two of them permanently.
func TestRemoveDeletedServers_SweepsThePerServerKeys(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()

	const gone = "owner_deadbeef01"
	const kept = "owner_stillalive"

	for _, k := range []string{
		"dylaris:server:" + gone + ":logs:survival",
		"dylaris:server:" + gone + ":logs:creative",
		"dylaris:server:" + gone + ":stats:buffer",
	} {
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: k, Values: map[string]any{"line": "x"}}).Err(); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	if err := rdb.Set(ctx, "dylaris:server:"+gone+":java-heap", "512", 0).Err(); err != nil {
		t.Fatalf("seed java-heap: %v", err)
	}
	// A surviving server, and a key that merely shares the prefix up to the uuid,
	// to prove the sweep is bounded by the exact uuid.
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:server:" + kept + ":logs:survival", Values: map[string]any{"line": "y"}}).Err(); err != nil {
		t.Fatalf("seed kept: %v", err)
	}
	if err := rdb.Set(ctx, "dylaris:server:"+gone+"_suffix:java-heap", "1", 0).Err(); err != nil {
		t.Fatalf("seed lookalike: %v", err)
	}

	RemoveDeletedServers(ctx, nil, rdb, []string{gone})

	for _, k := range []string{
		"dylaris:server:" + gone + ":logs:survival",
		"dylaris:server:" + gone + ":logs:creative",
		"dylaris:server:" + gone + ":stats:buffer",
		"dylaris:server:" + gone + ":java-heap",
	} {
		if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
			t.Errorf("%s still present after cleanup", k)
		}
	}
	for _, k := range []string{
		"dylaris:server:" + kept + ":logs:survival",
		"dylaris:server:" + gone + "_suffix:java-heap",
	} {
		if n, _ := rdb.Exists(ctx, k).Result(); n != 1 {
			t.Errorf("%s was removed; the sweep must be bounded by the exact uuid", k)
		}
	}
}

// An empty uuid must sweep nothing: "dylaris:server::*" would be harmless here,
// but the guard also covers a caller that lost the value.
func TestRemoveDeletedServers_EmptyUUIDSweepsNothing(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	if err := rdb.Set(ctx, "dylaris:server:owner_x:java-heap", "1", 0).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	RemoveDeletedServers(ctx, nil, rdb, []string{"   ", ""})

	if n, _ := rdb.Exists(ctx, "dylaris:server:owner_x:java-heap").Result(); n != 1 {
		t.Error("an empty uuid removed keys")
	}
}

// The regression this exists for: a cache that lost its route keys. The
// per-domain deletes can only name routes Redis still holds, so nothing reached
// the hub, its rows survived, and its next sync published the addresses again.
// The hub now hears about every deleted server regardless.
func TestRemoveDeletedServersTellsTheHubEvenWhenRedisIsEmpty(t *testing.T) {
	rdb := newQueueTestRedis(t)
	gw := &routeDeleteGateway{}

	RemoveDeletedServers(context.Background(), gw, rdb, []string{"srv-1", "srv-2"})

	if len(gw.serverDeletes) != 2 || gw.serverDeletes[0] != "srv-1" || gw.serverDeletes[1] != "srv-2" {
		t.Errorf("server deletes sent = %v, want both servers", gw.serverDeletes)
	}
	if len(gw.deleted) != 0 {
		t.Errorf("per-domain deletes = %v, want none: the cache named nothing", gw.deleted)
	}
}
