package main

import (
	"archive/tar"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// ext4's chattr sets the project id on the inodes it is NAMED, so without -R a
// server directory that already held data counted only bytes written later.
// xfs `project -s` walks the tree on its own.
func TestProjectAssignCommandTagsTheWholeTree(t *testing.T) {
	for _, tc := range []struct {
		fs   string
		want []string
	}{
		{"ext4", []string{"chattr", "-R", "+P", "-p", "7", "/srv/x"}},
		{"xfs", []string{"xfs_quota", "-x", "-c", "project -s -p /srv/x 7", "/mnt"}},
	} {
		t.Run(tc.fs, func(t *testing.T) {
			got := projectAssignCmd(tc.fs, "/srv/x", 7, "/mnt").Args
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeQuota reports `before` until AssignQuota ran and `after` from then on,
// which is what re-tagging an already populated directory does to the count.
type fakeQuota struct {
	before, after *DiskUsagePayload
	assigned      bool
}

func (f *fakeQuota) AssignQuota(string) error { f.assigned = true; return nil }
func (f *fakeQuota) GetDiskUsage(string) *DiskUsagePayload {
	if f.assigned {
		return f.after
	}
	return f.before
}

func usageOf(total, limit int64) *DiskUsagePayload {
	return &DiskUsagePayload{Total: total, Limit: limit}
}

// The owner's rule: a server that lands over its limit only because its
// existing data is now counted is warned, not stopped, the first time.
func TestAssignQuotaCountedGrantsGraceOnlyWhenTheRecountTipsIt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after *DiskUsagePayload
		grace         bool
	}{
		{"recount tips it over", usageOf(10, 100), usageOf(150, 100), true},
		{"untracked before (not in the report)", nil, usageOf(150, 100), true},
		{"recount stays under", usageOf(10, 100), usageOf(90, 100), false},
		{"already over before the recount", usageOf(120, 100), usageOf(150, 100), false},
		{"no limit", usageOf(10, 0), usageOf(150, 0), false},
		{"quotas unavailable", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newDiskGuardRedis(t)
			ctx := context.Background()
			assignQuotaCounted(ctx, rdb, &fakeQuota{before: tc.before, after: tc.after}, "srv")
			n, _ := rdb.Exists(ctx, diskGraceKey("srv")).Result()
			if (n == 1) != tc.grace {
				t.Fatalf("grace set = %v, want %v", n == 1, tc.grace)
			}
		})
	}
}

// Grace holds only while the server is still over the SAME limit; it ends for
// good once usage drops below it or the limit changes.
func TestDiskGraced(t *testing.T) {
	for _, tc := range []struct {
		name      string
		granted   bool
		usage     *DiskUsagePayload
		graced    bool
		keyRemain bool
	}{
		{"granted, still over the same limit", true, usageOf(150, 100), true, true},
		{"granted, usage dropped below", true, usageOf(90, 100), false, false},
		{"granted, limit changed", true, usageOf(150, 120), false, false},
		{"granted, limit removed", true, usageOf(150, 0), false, false},
		{"never granted", false, usageOf(150, 100), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newDiskGuardRedis(t)
			ctx := context.Background()
			if tc.granted {
				rdb.Set(ctx, diskGraceKey("srv"), 100, 0)
			}
			if got := diskGraced(ctx, rdb, "srv", tc.usage); got != tc.graced {
				t.Fatalf("graced = %v, want %v", got, tc.graced)
			}
			n, _ := rdb.Exists(ctx, diskGraceKey("srv")).Result()
			if (n == 1) != tc.keyRemain {
				t.Fatalf("grace key kept = %v, want %v", n == 1, tc.keyRemain)
			}
		})
	}
}

// A full restore swaps in a tree no project owns; the server has to be put
// back under its quota or it reads ~0 and its limit caps nothing.
func TestAFullRestoreRetagsTheServer(t *testing.T) {
	var got []string
	prev := retagAfterRestore
	t.Cleanup(func() { retagAfterRestore = prev })
	retagAfterRestore = func(_ context.Context, _ *redis.Client, uuid string) { got = append(got, uuid) }

	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"world/r.0.0.mca": "region bytes"})
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	if len(got) != 1 || got[0] != cmd.ServerUUID {
		t.Fatalf("retagged %v, want [%s]", got, cmd.ServerUUID)
	}
}

// A server that moved away must not leave its limit behind: a later move back
// with "unlimited" (0) fell back to that stale cap.
func TestMovingAwayForgetsTheDiskLimit(t *testing.T) {
	prevNodeID := nodeID
	t.Cleanup(func() { nodeID = prevNodeID })
	nodeID = "node-1"
	ctx := context.Background()

	t.Run("migrate_cleanup", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		root := t.TempDir()
		const uuid = "srv-gone"
		if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
			t.Fatal(err)
		}
		recordDiskLimit(ctx, rdb, uuid, 10240)
		rdb.Set(ctx, diskGraceKey(uuid), 1, 0)
		handleMigrateCleanup(ctx, rdb, &StorageManager{paths: []string{root}, rdb: rdb}, nil, "tok", uuid)
		if mr.Exists(diskLimitKey(uuid)) || mr.Exists(diskGraceKey(uuid)) {
			t.Fatal("the source kept the moved server's disk limit")
		}
	})

	t.Run("migrate_discard", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, discardUUID), 0o755); err != nil {
			t.Fatal(err)
		}
		markMovedIn(root, discardUUID, "attempt-9")
		recordDiskLimit(ctx, rdb, discardUUID, 10240)
		handleMigrateDiscard(&StorageManager{paths: []string{root}, rdb: rdb}, nil, discardUUID, "attempt-9", time.Second)
		for i := 0; i < 100 && mr.Exists(diskLimitKey(discardUUID)); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if mr.Exists(diskLimitKey(discardUUID)) {
			t.Fatal("the discarded copy kept its disk limit")
		}
	})
}

// The copy of a storage move takes minutes; holding the manager's write lock
// through it stalled the heartbeat and every other server's path lookup.
func TestMigrateServerPathDoesNotBlockLookupsDuringTheCopy(t *testing.T) {
	sm, _ := newPlacementManager(t, 2)
	paths := sm.Paths()
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7"
	const other = "c083ff0c-fc16-47c5-9700-7b16486583e7"
	if err := os.MkdirAll(filepath.Join(paths[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}

	copying, release := make(chan struct{}), make(chan struct{})
	prev := migrateCopyTree
	t.Cleanup(func() { migrateCopyTree = prev })
	migrateCopyTree = func(src, dst string) error {
		close(copying)
		<-release
		return prev(src, dst)
	}
	done := make(chan error, 1)
	go func() { done <- sm.MigrateServerPath(uuid, paths[1]) }()
	<-copying

	looked := make(chan struct{})
	go func() {
		sm.GetStorageInfo()
		sm.GetServerPath(other)
		close(looked)
	}()
	select {
	case <-looked:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("a lookup blocked while the copy ran")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("MigrateServerPath: %v", err)
	}
}

// F60: removing the limit of a held server is the strongest "raise the limit"
// and has to lift the hold. F62: a server still over keeps its hold alive
// rather than lapsing after the marker's TTL.
func TestReleaseResolvedDiskHolds(t *testing.T) {
	prevNodeID, prevMgr := nodeID, globalStorageMgr
	t.Cleanup(func() { nodeID, globalStorageMgr = prevNodeID, prevMgr })
	nodeID = "node-1"
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		limitMB  int64
		size     int
		released bool
	}{
		{"limit removed", 0, 4096, true},
		{"still over", 1, 2 << 20, false},
		{"back under", 1, 4096, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			root := t.TempDir()
			globalStorageMgr = NewStorageManager(root, nil)
			const held = "cccccccc-1111-2222-3333-444444444444"
			if err := os.MkdirAll(filepath.Join(root, held), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, held, "world.dat"), make([]byte, tc.size), 0o644); err != nil {
				t.Fatal(err)
			}
			recordDiskLimit(ctx, rdb, held, tc.limitMB)
			mr.Set(diskFullKey(held), "1")
			mr.SetTTL(diskFullKey(held), time.Minute) // most of the hour already gone

			releaseResolvedDiskHolds(ctx, rdb, NewQuotaSet(nil))

			if released := !mr.Exists(diskFullKey(held)); released != tc.released {
				t.Fatalf("released = %v, want %v", released, tc.released)
			}
			if !tc.released {
				if ttl := mr.TTL(diskFullKey(held)); ttl < diskFullMarkerTTL-time.Minute {
					t.Fatalf("hold TTL = %v, want it refreshed to %v", ttl, diskFullMarkerTTL)
				}
			} else if got, _ := mr.Get(fmt.Sprintf("dylaris:server:%s:status", held)); got != "stopped" {
				t.Fatalf("status = %q, want stopped", got)
			}
		})
	}
}
