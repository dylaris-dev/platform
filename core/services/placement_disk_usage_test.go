package services

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The gauge is read per server from the node's disk report; a missing or
// unparseable entry must be absent (whole limit stays reserved), not zero-filled
// into something that looks measured.
func TestLoadServerDiskUsage(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	mr.Set("dylaris:server:a:stats:disk", `{"total":18200000000,"limit":85899345920,"subServers":{}}`)
	mr.Set("dylaris:server:b:stats:disk", `not json`)

	storage := []HeartbeatStoragePath{{Path: "/p1", ServerUUIDs: []string{"a"}}, {Path: "/p2", ServerUUIDs: []string{"b", "c"}}}
	got := LoadServerDiskUsage(context.Background(), rdb, storage)
	if len(got) != 1 || got["a"] != 18200000000 {
		t.Errorf("LoadServerDiskUsage = %v, want only a=18200000000", got)
	}
	if got := LoadServerDiskUsage(context.Background(), nil, storage); len(got) != 0 {
		t.Errorf("nil redis = %v, want empty", got)
	}
}
