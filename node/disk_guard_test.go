package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newDiskGuardRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRecordAndLoadDiskLimit(t *testing.T) {
	prevNodeID := nodeID
	t.Cleanup(func() { nodeID = prevNodeID })
	nodeID = "node-1"

	ctx := context.Background()
	rdb := newDiskGuardRedis(t)
	const uuid = "srv-1"

	if got := loadDiskLimit(ctx, rdb, uuid); got != 0 {
		t.Errorf("unset limit = %d, want 0", got)
	}

	recordDiskLimit(ctx, rdb, uuid, 51200)
	if got := loadDiskLimit(ctx, rdb, uuid); got != 51200 {
		t.Errorf("limit = %d, want 51200", got)
	}

	// Raising a limit is one of the two documented ways out of a disk_full
	// hold, so the cache must follow it rather than keep the old value.
	recordDiskLimit(ctx, rdb, uuid, 102400)
	if got := loadDiskLimit(ctx, rdb, uuid); got != 102400 {
		t.Errorf("raised limit = %d, want 102400", got)
	}

	// 0 means unlimited. A stale cached limit here would keep stopping a server
	// whose limit was deliberately removed.
	recordDiskLimit(ctx, rdb, uuid, 0)
	if got := loadDiskLimit(ctx, rdb, uuid); got != 0 {
		t.Errorf("cleared limit = %d, want 0", got)
	}
}

func TestDiskLimitIsPerServerAndPerNode(t *testing.T) {
	prevNodeID := nodeID
	t.Cleanup(func() { nodeID = prevNodeID })

	ctx := context.Background()
	rdb := newDiskGuardRedis(t)

	nodeID = "node-1"
	recordDiskLimit(ctx, rdb, "srv-1", 1000)

	if got := loadDiskLimit(ctx, rdb, "srv-2"); got != 0 {
		t.Errorf("another server's limit leaked: %d", got)
	}
	nodeID = "node-2"
	if got := loadDiskLimit(ctx, rdb, "srv-1"); got != 0 {
		t.Errorf("another node's limit leaked: %d", got)
	}
}

// The gap this closes: on a filesystem without project quotas the node reported
// NO disk usage at all, so the existing disk-full machinery never had a number
// to act on. du has to produce one, complete with the cached limit, or the
// thresholds downstream are computed against zero.
func TestDuDiskUsageReportsMeasuredSizeAndCachedLimit(t *testing.T) {
	prevNodeID, prevMgr := nodeID, globalStorageMgr
	t.Cleanup(func() { nodeID, globalStorageMgr = prevNodeID, prevMgr })
	nodeID = "node-1"

	ctx := context.Background()
	rdb := newDiskGuardRedis(t)
	const uuid = "srv-1-0001"

	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "world.dat"), make([]byte, 4096), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	recordDiskLimit(ctx, rdb, uuid, 100) // 100 MB

	usage := duDiskUsage(ctx, rdb, uuid)
	if usage == nil {
		t.Fatal("duDiskUsage returned nil; a quota-less filesystem must still report usage")
	}
	if usage.Total <= 0 {
		t.Errorf("Total = %d, want the measured size", usage.Total)
	}
	if want := int64(100) * 1024 * 1024; usage.Limit != want {
		t.Errorf("Limit = %d, want %d - without it the disk-full thresholds compare against zero", usage.Limit, want)
	}
}

func TestDuDiskUsageWithoutStorageManager(t *testing.T) {
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	globalStorageMgr = nil

	if got := duDiskUsage(context.Background(), nil, "srv-1"); got != nil {
		t.Errorf("duDiskUsage without a storage manager = %+v, want nil", got)
	}
}
