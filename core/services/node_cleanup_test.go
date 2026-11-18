package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type staleNodeStore struct {
	store.Store
	tokens []string
}

func (s staleNodeStore) DeleteStaleOfflineNodes(time.Time) ([]string, error) {
	return s.tokens, nil
}

// The sweep deleted the rows in SQL and nothing else, so every swept node left
// its command stream, stats streams and per-server keys in Redis for good -
// none of them expire. It must tear down what every other delete path does.
func TestNodeCleanupRemovesTheSweptNodesRedisState(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	for _, k := range []string{
		"dylaris:node:gone:cmds",
		"dylaris:node:gone:system",
		"node:gone:server:u1:storage",
		"dylaris:node:kept:cmds",
	} {
		if err := rdb.Set(ctx, k, "x", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}

	s := NewNodeCleanupService(staleNodeStore{tokens: []string{"gone"}}, rdb, time.Hour)
	s.sweep(ctx)

	for k, want := range map[string]bool{
		"dylaris:node:gone:cmds":      false,
		"dylaris:node:gone:system":    false,
		"node:gone:server:u1:storage": false,
		"dylaris:node:kept:cmds":      true,
	} {
		if got := mr.Exists(k); got != want {
			t.Errorf("%s exists = %v, want %v", k, got, want)
		}
	}
}
