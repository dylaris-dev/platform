package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// H10/H51/H54: a recreate under the same name keeps the collector, so the
// first sample of the new container is taken against the old one's counters.
// The new cgroup starts near 0 and the uint64 difference wrapped to ~1.8e19,
// which the collector clamped to a false 100% CPU sample.
func TestCPUPercentSince(t *testing.T) {
	for _, tc := range []struct {
		name string
		prev *PrevCPUStats
		cur  PrevCPUStats
		want float64
	}{
		{"no baseline", nil, PrevCPUStats{TotalUsage: 100, SystemUsage: 1000, ContainerID: "a"}, 0},
		{"same container", &PrevCPUStats{TotalUsage: 100, SystemUsage: 1000, ContainerID: "a"}, PrevCPUStats{TotalUsage: 150, SystemUsage: 1100, ContainerID: "a"}, 200},
		{"counter went backwards", &PrevCPUStats{TotalUsage: 5e12, SystemUsage: 1000, ContainerID: "a"}, PrevCPUStats{TotalUsage: 10, SystemUsage: 1100, ContainerID: "a"}, 0},
		{"system went backwards", &PrevCPUStats{TotalUsage: 100, SystemUsage: 1000, ContainerID: "a"}, PrevCPUStats{TotalUsage: 150, SystemUsage: 900, ContainerID: "a"}, 0},
		// Counters of a short-lived old container can sit below the new one's.
		{"container replaced", &PrevCPUStats{TotalUsage: 10, SystemUsage: 1000, ContainerID: "a"}, PrevCPUStats{TotalUsage: 90, SystemUsage: 1100, ContainerID: "b"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cpuPercentSince(tc.prev, tc.cur, 4); got != tc.want {
				t.Fatalf("cpuPercentSince = %v, want %v", got, tc.want)
			}
		})
	}
}

func heldServer(t *testing.T, sizeBytes int) (*miniredis.Miniredis, *redis.Client, string) {
	t.Helper()
	prevNodeID, prevMgr := nodeID, globalStorageMgr
	t.Cleanup(func() { nodeID, globalStorageMgr = prevNodeID, prevMgr })
	nodeID = "node-1"
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	const held = "cccccccc-1111-2222-3333-444444444444"
	if err := os.MkdirAll(filepath.Join(root, held), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, held, "world.dat"), make([]byte, sizeBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	recordDiskLimit(context.Background(), rdb, held, 1)
	mr.Set(diskFullKey(held), "1")
	mr.SetTTL(diskFullKey(held), time.Minute)
	return mr, rdb, held
}

// H53/H56: a failed container list is "not known", not "nothing runs". Treated
// as the latter, the sweep refreshed the marker of a RUNNING held server and
// pushed its collector's next trip out by another hour.
func TestReleaseResolvedDiskHoldsUnknownRunningIsSkipped(t *testing.T) {
	mr, rdb, held := heldServer(t, 2<<20)
	releaseResolvedDiskHolds(context.Background(), rdb, NewQuotaSet(nil), nil)
	if ttl := mr.TTL(diskFullKey(held)); ttl != time.Minute {
		t.Fatalf("marker TTL = %v, want it untouched (1m)", ttl)
	}
}

// H52: the sweep re-posts disk_full for a stopped server still over its limit,
// so a lost trip write heals within one sweep instead of leaving "stopping".
func TestReleaseResolvedDiskHoldsRepostsDiskFull(t *testing.T) {
	mr, rdb, held := heldServer(t, 2<<20)
	releaseResolvedDiskHolds(context.Background(), rdb, NewQuotaSet(nil), map[string]bool{})
	if got, _ := mr.Get(fmt.Sprintf("dylaris:server:%s:status", held)); got != "disk_full" {
		t.Fatalf("status = %q, want disk_full", got)
	}
}

// H52: the scan cancels the collector's context once the container is gone,
// which is exactly while the guard's stop returns. The writes after the stop
// must not ride on that context.
func TestHoldForDiskFullSurvivesTheCollectorCancel(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	const uuid = "dddddddd-1111-2222-3333-444444444444"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := rdb.Subscribe(context.Background(), "live")
	defer sub.Close()
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}

	holdForDiskFull(ctx, rdb, uuid, "live", &DiskUsagePayload{Total: 2, Limit: 1}, cancel)

	if !mr.Exists(diskFullKey(uuid)) {
		t.Fatal("marker not set")
	}
	if got, _ := mr.Get(fmt.Sprintf("dylaris:server:%s:status", uuid)); got != "disk_full" {
		t.Fatalf("status = %q, want disk_full", got)
	}
	select {
	case <-sub.Channel():
	case <-time.After(2 * time.Second):
		t.Fatal("disk_warning not published")
	}
}

// H50: the running collector released a hold and reported "stopped" for a
// container that was still running.
func TestReleaseDiskHoldFromCollector(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		status  string
	}{
		{"still running", true, ""},
		{"already stopped", false, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			const uuid = "eeeeeeee-1111-2222-3333-444444444444"
			mr.Set(diskFullKey(uuid), "1")

			releaseDiskHold(context.Background(), rdb, uuid, "live", &DiskUsagePayload{Total: 1, Limit: 2}, tc.running)

			if mr.Exists(diskFullKey(uuid)) {
				t.Fatal("marker not released")
			}
			if got, _ := mr.Get(fmt.Sprintf("dylaris:server:%s:status", uuid)); got != tc.status {
				t.Fatalf("status = %q, want %q", got, tc.status)
			}
		})
	}
}
