package stats

import (
	"context"
	"testing"
	"time"

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

	if err := rdb.Set(context.Background(), key, "1", 10*time.Second).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// What the handler holds at defer time.
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rdb.Del(reqCtx, key).Err(); err == nil {
		t.Fatal("a Del on a cancelled context succeeded - the premise of this test is wrong")
	}
	if n, _ := rdb.Exists(context.Background(), key).Result(); n != 1 {
		t.Fatal("the cancelled Del removed the key after all")
	}

	clearStatsWatch(rdb, key)

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
	clearStatsWatch(nil, "some:key")
}
