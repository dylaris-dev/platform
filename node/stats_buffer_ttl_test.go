package main

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// G48: Core deletes a server's keys right after it queues the delete, and the
// collector, still running until the container is gone, wrote the buffer back
// with no TTL - an orphan nothing ever removed. Every write arms the TTL, so a
// stream nobody writes any more lapses on its own.
func TestStatsSampleArmsTheBufferTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	const key = "dylaris:server:srv-1:stats:buffer"

	for i := 0; i < 2; i++ {
		appendStatsSample(ctx, rdb, key, 10, []byte(`{"players":0}`))
		if ttl := mr.TTL(key); ttl <= 0 || ttl > statsBufferTTL {
			t.Fatalf("write %d: buffer TTL = %v, want it armed to %v", i, ttl, statsBufferTTL)
		}
	}
	if n, _ := rdb.XLen(ctx, key).Result(); n != 2 {
		t.Fatalf("buffer holds %d samples, want 2", n)
	}
}
