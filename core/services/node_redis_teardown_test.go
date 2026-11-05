package services

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The node writes its port ledger without a TTL and only its own release
// removes an entry, so a node deleted while offline left every key of it for
// good, and the status watcher read each one on every pass. Another node's
// ledger stays.
func TestRemoveNodeRedisStateDropsThePortLedger(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	for _, k := range []string{"dylaris:node:tok:port:srv-a", "dylaris:node:tok:port:srv-b", "dylaris:node:other:port:srv-c"} {
		mr.Set(k, "25600")
	}
	mr.HSet("dylaris:node:tok:ports", "srv-a", "25600", "srv-b", "25601")
	mr.HSet("dylaris:node:other:ports", "srv-c", "25600")

	RemoveNodeRedisState(t.Context(), rdb, nil, "tok")

	for _, k := range []struct {
		key  string
		want bool
	}{
		{"dylaris:node:tok:port:srv-a", false},
		{"dylaris:node:tok:port:srv-b", false},
		{"dylaris:node:tok:ports", false},
		{"dylaris:node:other:port:srv-c", true},
		{"dylaris:node:other:ports", true},
	} {
		if got := mr.Exists(k.key); got != k.want {
			t.Errorf("%s exists = %v, want %v", k.key, got, k.want)
		}
	}
}
