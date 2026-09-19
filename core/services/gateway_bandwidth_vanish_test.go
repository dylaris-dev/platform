package services

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A revoked link's stats stream is deleted with it. The consumer used to heal
// every read error by recreating the stream, which kept a dead kit's stream and
// a goroutine per Core alive forever. A stream that is gone now ends its
// consumer, and is left gone.
func TestConsumerLetsAVanishedStreamGo(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := NewGatewayBandwidthConsumerService(nil, rdb, "core-a")
	const key = "dylaris:link:link-dead:stats"
	s.streams[key] = true

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.consume(ctx, key); close(done) }()

	// Let it create its group and block, then take the stream away.
	time.Sleep(200 * time.Millisecond)
	mr.Del(key)

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the consumer kept running on a stream that no longer exists")
	}
	if mr.Exists(key) {
		t.Error("the consumer recreated the stream it was reading")
	}
	s.mu.Lock()
	still := s.streams[key]
	s.mu.Unlock()
	if still {
		t.Error("the stream stayed registered, so a returning one would never be consumed again")
	}
}
