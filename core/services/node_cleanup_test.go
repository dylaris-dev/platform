package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
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

type orphanSweepStore struct {
	store.Store
	nodes  []models.Node
	onList func()
}

func (s *orphanSweepStore) ListNodes() ([]models.Node, error) {
	if s.onList != nil {
		s.onList()
	}
	return s.nodes, nil
}

// A publisher that listed a node before its row went can SET the node's keys
// again after the teardown's DEL, with no TTL. The sweep removes keys whose
// token has no row, and only those.
func TestNodeCleanupSweepsOrphanPublisherKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	for _, k := range []string{
		NetPolicyKey("gone"),
		NetPolicyTenancyKey("gone"),
		"dylaris:node:gone:storage_placement",
		NodeBackupPolicyKey("gone"),
		NetPolicyKey("live"),
		NetPolicyTenancyKey("live"),
		"dylaris:node:live:storage_placement",
		NodeBackupPolicyKey("live"),
		// Owned by the node itself, not a publisher: left to the teardown.
		"dylaris:node:gone:cmds",
	} {
		if err := rdb.Set(ctx, k, "x", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	st := &orphanSweepStore{nodes: []models.Node{{Token: "live"}}}
	// A node registered after the scan but before the list: its key must stay.
	st.onList = func() {
		rdb.Set(ctx, NodeBackupPolicyKey("new"), "x", 0)
		st.nodes = append(st.nodes, models.Node{Token: "new"})
	}

	NewNodeCleanupService(st, rdb, time.Hour).sweepOrphanKeys(ctx)

	for k, want := range map[string]bool{
		NetPolicyKey("gone"):                  false,
		NetPolicyTenancyKey("gone"):           false,
		"dylaris:node:gone:storage_placement": false,
		NodeBackupPolicyKey("gone"):           false,
		NetPolicyKey("live"):                  true,
		NetPolicyTenancyKey("live"):           true,
		"dylaris:node:live:storage_placement": true,
		NodeBackupPolicyKey("live"):           true,
		NodeBackupPolicyKey("new"):            true,
		"dylaris:node:gone:cmds":              true,
	} {
		if got := mr.Exists(k); got != want {
			t.Errorf("%s exists = %v, want %v", k, got, want)
		}
	}
}
