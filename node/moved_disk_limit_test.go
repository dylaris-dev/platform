package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A server that moved here ran with no disk limit: the move commands carried
// none and the node registered nothing for the arriving directory.
func TestAMovedServerKeepsItsDiskLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	quota := NewQuotaSet(nil)
	const uuid = "moved-1"
	dir := t.TempDir()

	applyMovedDiskLimit(ctx, rdb, quota, dir, uuid, 2048)
	if got := loadDiskLimit(ctx, rdb, uuid); got != 2048 {
		t.Fatalf("limit after the move = %d, want 2048", got)
	}

	// An older Core sends no limit. That means "not told", and must not wipe
	// the limit this node already enforces.
	applyMovedDiskLimit(ctx, rdb, quota, dir, uuid, 0)
	if got := loadDiskLimit(ctx, rdb, uuid); got != 2048 {
		t.Fatalf("limit after a move that named none = %d, want the known 2048 kept", got)
	}

	// A failed move leaves no directory, and nothing is recorded for it.
	applyMovedDiskLimit(ctx, rdb, quota, dir+"/missing", "never-arrived", 512)
	if got := loadDiskLimit(ctx, rdb, "never-arrived"); got != 0 {
		t.Fatalf("a server that never arrived got limit %d", got)
	}
}

// Usage was measured only for running containers, and every upload check lets
// an upload through when it finds none - so a stopped server took any amount.
func TestStoppedServersPublishTheirDiskUsage(t *testing.T) {
	prevNodeID, prevMgr := nodeID, globalStorageMgr
	t.Cleanup(func() { nodeID, globalStorageMgr = prevNodeID, prevMgr })
	nodeID = "node-1"
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	const stopped = "aaaaaaaa-1111-2222-3333-444444444444"
	const running = "bbbbbbbb-1111-2222-3333-444444444444"
	// The id the panel actually mints: "<ownerUUID>_<random>". Every real
	// server has this shape, and the sweep used to skip all of them.
	const minted = "cccccccc-1111-2222-3333-444444444444_232qs8ryxy3"
	for _, d := range []string{stopped, running, minted, "not-a-server"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "world.dat"), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recordDiskLimit(ctx, rdb, stopped, 1)
	recordDiskLimit(ctx, rdb, minted, 1)

	publishStoppedDiskUsage(ctx, rdb, NewQuotaSet(nil), map[string]bool{running: true})

	raw, err := rdb.Get(ctx, "dylaris:server:"+stopped+":stats:disk").Result()
	if err != nil {
		t.Fatalf("no disk usage published for the stopped server: %v", err)
	}
	var usage DiskUsagePayload
	if err := json.Unmarshal([]byte(raw), &usage); err != nil || usage.Total < 4096 || usage.Limit != 1024*1024 {
		t.Fatalf("published usage = %s (%v), want total >= 4096 and the 1 MB limit", raw, err)
	}
	if mr.Exists("dylaris:server:" + running + ":stats:disk") {
		t.Error("the running server's own collector is the one that measures it")
	}
	if !mr.Exists("dylaris:server:" + minted + ":stats:disk") {
		t.Error("a server with the panel's minted id was skipped")
	}
	if mr.Exists("dylaris:server:not-a-server:stats:disk") {
		t.Error("a directory that is not a server was measured")
	}
}
