package stats

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newWatchTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// The watch key tells the node to keep publishing live stats. It is cleared from
// a deferred call in an SSE handler whose normal exit is the client
// disconnecting - so the REQUEST context is already cancelled when the cleanup
// runs. go-redis checks ctx.Err() before dispatching, so a Del on that context
// never reaches Redis and the key survives to its TTL while the node keeps
// publishing to nobody.
//
// This drives the cleanup exactly that way: cancelled request context, key must
// still go. It fails against a cleanup that reuses r.Context().
func TestClearStatsWatch_RunsWithACancelledRequestContext(t *testing.T) {
	rdb := newWatchTestRedis(t)
	const key = "dylaris:server:abc:stats:watching"

	markStatsWatch(context.Background(), rdb, key, "viewer")

	// What the handler holds at defer time.
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rdb.Del(reqCtx, key).Err(); err == nil {
		t.Fatal("a Del on a cancelled context succeeded - the premise of this test is wrong")
	}
	if n, _ := rdb.Exists(context.Background(), key).Result(); n != 1 {
		t.Fatal("the cancelled Del removed the key after all")
	}

	clearStatsWatch(rdb, key, "viewer")

	n, err := rdb.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if n != 0 {
		t.Error("watch key survived the cleanup - the node keeps publishing until the TTL expires")
	}
}

// A nil Redis is the no-Redis deployment, not a crash.
func TestClearStatsWatch_NilRedisIsANoOp(t *testing.T) {
	clearStatsWatch(nil, "some:key", "viewer")
}

// H55: every viewer of a server shares one watch key. One leaving deleted it,
// and the node stopped publishing to the viewers still watching until their
// next refresh, freezing their live chart for up to ~7 s.
func TestClearStatsWatch_OtherViewersKeepTheKey(t *testing.T) {
	rdb := newWatchTestRedis(t)
	ctx := context.Background()
	const key = "dylaris:server:abc:stats:watching"

	markStatsWatch(ctx, rdb, key, "viewer-a")
	markStatsWatch(ctx, rdb, key, "viewer-b")

	clearStatsWatch(rdb, key, "viewer-a")
	if n, _ := rdb.Exists(ctx, key).Result(); n != 1 {
		t.Fatal("one viewer leaving cleared the watch key for the other")
	}
	if ttl, _ := rdb.TTL(ctx, key).Result(); ttl <= 0 {
		t.Fatalf("watch key has no TTL (%v)", ttl)
	}

	clearStatsWatch(rdb, key, "viewer-b")
	if n, _ := rdb.Exists(ctx, key).Result(); n != 0 {
		t.Fatal("the last viewer leaving did not clear the watch key")
	}
}

// A Core one deploy behind writes the key as a string; a new viewer must still
// keep it alive rather than fail on WRONGTYPE.
func TestMarkStatsWatch_OldStringKeyStaysWatched(t *testing.T) {
	rdb := newWatchTestRedis(t)
	ctx := context.Background()
	const key = "dylaris:server:abc:stats:watching"
	rdb.Set(ctx, key, "1", 0)

	markStatsWatch(ctx, rdb, key, "viewer-a")
	if ttl, _ := rdb.TTL(ctx, key).Result(); ttl <= 0 {
		t.Fatalf("old-shape key not refreshed (TTL %v)", ttl)
	}
}
