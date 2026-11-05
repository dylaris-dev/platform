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
// It records the hard limit in force when the tag ran and every limit set.
type fakeQuota struct {
	before, after *DiskUsagePayload
	assigned      bool
	limitMB       int64
	limitAtTag    int64
	setLimits     []int64
	// onTag runs inside the tag, where a node killed mid-walk stops.
	onTag func()
}

func (f *fakeQuota) AssignQuota(string) error {
	f.assigned = true
	f.limitAtTag = f.limitMB
	if f.onTag != nil {
		f.onTag()
	}
	return nil
}
func (f *fakeQuota) GetDiskUsage(string) *DiskUsagePayload {
	if f.assigned {
		return f.after
	}
	return f.before
}
func (f *fakeQuota) SetLimit(_ string, limitMB int64) error {
	f.limitMB = limitMB
	f.setLimits = append(f.setLimits, limitMB)
	return nil
}

func usageOf(total, limit int64) *DiskUsagePayload {
	return &DiskUsagePayload{Total: total, Limit: limit}
}

const mib = int64(1) << 20

// The owner's rule: a server that lands over its limit only because its
// existing data is now counted is warned, not stopped, the first time - and
// keeps working, so its filesystem limit is lifted to usage plus headroom.
// Only the boot recount can grant it (G36/G38): a move, a restore or a storage
// move of a tree that is over its limit is an ordinary over-limit server.
func TestCountDiskUsageGrantsGraceOnlyWhenTheRecountTipsIt(t *testing.T) {
	ceiling := (150*mib + diskGraceHeadroom(100*mib) + mib - 1) / mib
	for _, tc := range []struct {
		name          string
		before, after *DiskUsagePayload
		allowGrace    bool
		grace         bool
		enforcedMB    int64
	}{
		{"recount tips it over", usageOf(10*mib, 100*mib), usageOf(150*mib, 100*mib), true, true, ceiling},
		{"untracked before (not in the report)", nil, usageOf(150*mib, 100*mib), true, true, ceiling},
		{"recount stays under", usageOf(10*mib, 100*mib), usageOf(90*mib, 100*mib), true, false, 100},
		{"already over before the recount", usageOf(120*mib, 100*mib), usageOf(150*mib, 100*mib), true, false, 100},
		{"move, restore or storage move over its limit", nil, usageOf(150*mib, 100*mib), false, false, 100},
		{"quotas unavailable", nil, nil, true, false, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newDiskGuardRedis(t)
			ctx := context.Background()
			q := &fakeQuota{before: tc.before, after: tc.after, limitMB: 100}
			countDiskUsage(ctx, rdb, q, "srv", 100, tc.allowGrace)
			n, _ := rdb.Exists(ctx, diskGraceKey("srv")).Result()
			if (n == 1) != tc.grace {
				t.Fatalf("grace set = %v, want %v", n == 1, tc.grace)
			}
			// G35: ext4 refuses to charge re-tagged inodes past bhard without
			// CAP_SYS_RESOURCE, so the tag has to run with no hard limit.
			if q.limitAtTag != 0 {
				t.Fatalf("hard limit during the tag = %d MB, want it lifted", q.limitAtTag)
			}
			if q.limitMB != tc.enforcedMB {
				t.Fatalf("hard limit after = %d MB, want %d", q.limitMB, tc.enforcedMB)
			}
		})
	}

	t.Run("no limit leaves the filesystem limit alone", func(t *testing.T) {
		q := &fakeQuota{after: usageOf(150*mib, 0)}
		countDiskUsage(context.Background(), newDiskGuardRedis(t), q, "srv", 0, true)
		if len(q.setLimits) != 0 {
			t.Fatalf("set limits %v, want none", q.setLimits)
		}
	})
}

// A grace already granted survives a later recount under the same limit (a
// storage move, a restore) with its ceiling, is not granted again, and ends
// when the limit changes or usage is back under it.
//
// H39: a count that cannot be read after the tag is not the server getting
// back under its limit; the real limit on the filesystem with the grace key
// still in place had the guard let a server run that could not write.
func TestCountDiskUsageKeepsAnExistingGraceOnlyUnderTheSameLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limitMB    int64
		after      *DiskUsagePayload
		kept       bool
		enforcedMB int64
	}{
		{"same limit, still over", 100, usageOf(150*mib, 0), true, 300},
		{"limit raised", 200, usageOf(150*mib, 0), false, 200},
		{"back under", 100, usageOf(90*mib, 0), false, 100},
		{"same limit, count unreadable", 100, nil, true, 300},
		{"limit raised, count unreadable", 200, nil, false, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newDiskGuardRedis(t)
			ctx := context.Background()
			saveDiskGrace(ctx, rdb, "srv", diskGrace{limit: 100 * mib, ceiling: 300 * mib})
			q := &fakeQuota{after: tc.after}
			countDiskUsage(ctx, rdb, q, "srv", tc.limitMB, false)
			if _, ok := loadDiskGrace(ctx, rdb, "srv"); ok != tc.kept {
				t.Fatalf("grace kept = %v, want %v", ok, tc.kept)
			}
			if q.limitMB != tc.enforcedMB {
				t.Fatalf("hard limit = %d MB, want %d", q.limitMB, tc.enforcedMB)
			}
		})
	}
}

// Grace holds only while the server is still over the SAME limit and inside
// its headroom; once it ends the real limit goes back on the filesystem.
//
// H42: a grace whose limit CHANGED is obsolete, not used up. The new limit's
// writer recounts and sets it; a tick inside that recount stopped the server
// the owner had just given more disk, judged against the old limit, and put a
// hard limit on mid-tag. H44: a limit that cannot be read is not "unlimited".
func TestDiskGraced(t *testing.T) {
	prevNodeID := nodeID
	t.Cleanup(func() { nodeID = prevNodeID })
	nodeID = "node-1"
	for _, tc := range []struct {
		name      string
		granted   bool
		limitMB   int64 // -1: the cached limit cannot be read
		total     int64
		graced    bool
		kept      bool
		restoreMB int64 // limit put back when the grace ends, -1 for none
	}{
		{"granted, still over the same limit", true, 100, 150 * mib, true, true, -1},
		{"granted, usage dropped below", true, 100, 90 * mib, false, false, 100},
		{"granted, limit changed", true, 120, 150 * mib, true, false, -1},
		{"granted, limit removed", true, 0, 150 * mib, true, false, -1},
		{"granted, headroom used up", true, 100, 300 * mib, false, false, 100},
		{"granted, limit unreadable", true, -1, 150 * mib, true, true, -1},
		{"never granted", false, 100, 150 * mib, false, false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			ctx := context.Background()
			if tc.limitMB < 0 {
				// A WRONGTYPE reply: a GET that errors rather than misses.
				mr.Lpush(diskLimitKey("srv"), "x")
			} else {
				recordDiskLimit(ctx, rdb, "srv", tc.limitMB)
			}
			if tc.granted {
				saveDiskGrace(ctx, rdb, "srv", diskGrace{limit: 100 * mib, ceiling: 300 * mib})
			}
			q := &fakeQuota{limitMB: -1}
			if got := diskGraced(ctx, rdb, q, "srv", usageOf(tc.total, 100*mib)); got != tc.graced {
				t.Fatalf("graced = %v, want %v", got, tc.graced)
			}
			if _, ok := loadDiskGrace(ctx, rdb, "srv"); ok != tc.kept {
				t.Fatalf("grace key kept = %v, want %v", ok, tc.kept)
			}
			if q.limitMB != tc.restoreMB {
				t.Fatalf("hard limit = %d MB, want %d", q.limitMB, tc.restoreMB)
			}
		})
	}
}

// While graced, the filesystem's hard limit is the ceiling; the usage the panel
// and the guard see still carries the server's own limit, so it reads "full".
func TestGracedUsageReportsTheServersLimit(t *testing.T) {
	rdb := newDiskGuardRedis(t)
	ctx := context.Background()
	saveDiskGrace(ctx, rdb, "srv", diskGrace{limit: 100 * mib, ceiling: 300 * mib})
	u := usageOf(150*mib, 300*mib)
	recordDiskLimit(ctx, rdb, "srv", 100)
	withGraceLimit(ctx, rdb, "srv", u)
	if u.Limit != 100*mib {
		t.Fatalf("limit = %d, want the granted %d", u.Limit, 100*mib)
	}

	// H42: once the limit changed the grace no longer speaks for the server;
	// reporting the old limit read as "full" right after a raise.
	recordDiskLimit(ctx, rdb, "srv", 200)
	u = usageOf(150*mib, 0)
	withGraceLimit(ctx, rdb, "srv", u)
	if u.Limit != 0 {
		t.Fatalf("limit = %d after the limit changed, want the filesystem's", u.Limit)
	}
}

// H38/H44/H43: the hard limit is off while the tag runs. A node killed then
// left it off for good when the limit lived only on the project (a server
// from before the cache), and a server already counted was never looked at
// again. The limit is cached and the lift marked before it is lifted; the
// next boot recounts a marked server and puts the limit back.
func TestARecountKilledMidTagGetsItsLimitBackOnBoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cached  bool
		counted bool
	}{
		{"server from before the cache, first boot", false, false},
		{"counted server, recount for a resize", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newDiskGuardRedis(t)
			ctx := context.Background()
			if tc.cached {
				recordDiskLimit(ctx, rdb, "srv", 100)
			}
			if tc.counted {
				rdb.Set(ctx, diskCountedKey("srv"), 1, 0)
			}
			// The project holds the limit until the recount lifts it.
			q := &fakeQuota{before: usageOf(10*mib, 100*mib), limitMB: 100,
				onTag: func() { panic("killed") }}
			func() {
				defer func() { _ = recover() }()
				countDiskUsage(ctx, rdb, q, "srv", knownDiskLimitMB(ctx, rdb, q, "srv"), !tc.counted)
			}()
			if q.limitMB != 0 {
				t.Fatalf("hard limit during the tag = %d, want lifted", q.limitMB)
			}

			// Next boot: the project reads the lifted 0.
			q2 := &fakeQuota{before: usageOf(10*mib, 0), after: usageOf(10*mib, 0)}
			if done, _ := reconcileServer(ctx, rdb, q2, "srv"); !done {
				t.Fatal("the boot skipped a server whose limit was left lifted")
			}
			if q2.limitMB != 100 {
				t.Fatalf("hard limit after the boot = %d MB, want 100", q2.limitMB)
			}
			if got := loadDiskLimit(ctx, rdb, "srv"); got != 100 {
				t.Fatalf("cached limit = %d, want 100 backfilled from the project", got)
			}
			q3 := &fakeQuota{after: usageOf(10*mib, 100*mib), limitMB: 100}
			if done, _ := reconcileServer(ctx, rdb, q3, "srv"); done {
				t.Fatal("a boot after a finished recount walked the server again")
			}
		})
	}
}

// H45: the boot recount took every directory on a path for the server, so a
// copy a storage move left behind was counted, set the once-marker, and the
// real tree on the other path was skipped.
func TestTheBootRecountCountsOnlyWhereTheServerLives(t *testing.T) {
	prevNodeID := nodeID
	t.Cleanup(func() { nodeID = prevNodeID })
	nodeID = "node-1"
	roots := []string{t.TempDir(), t.TempDir()}
	rdb := newDiskGuardRedis(t)
	sm := NewStorageManager(strings.Join(roots, ","), rdb)
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7"
	for _, r := range roots {
		if err := os.MkdirAll(filepath.Join(r, uuid), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sm.setRedisPath(uuid, roots[1])
	for i, r := range roots {
		got := (&QuotaProvider{basePath: r}).serverDirs(sm)
		if want := i == 1; (len(got) == 1 && got[0] == uuid) != want {
			t.Fatalf("path %d lists %v, want the server only where it lives", i, got)
		}
	}
}

// G39 and the once-rule: the boot recount walks a tree only until it has been
// counted on this node; after that a restart neither walks it again nor can
// grant the grace a second time.
func TestReconcileCountsAServerOnce(t *testing.T) {
	rdb := newDiskGuardRedis(t)
	ctx := context.Background()
	recordDiskLimit(ctx, rdb, "srv", 100)
	q := &fakeQuota{before: usageOf(10*mib, 100*mib), after: usageOf(150*mib, 100*mib), limitMB: 100}
	if done, _ := reconcileServer(ctx, rdb, q, "srv"); !done || !q.assigned {
		t.Fatal("the first boot did not count the server")
	}
	rdb.Del(ctx, diskGraceKey("srv"))
	q2 := &fakeQuota{before: usageOf(10*mib, 100*mib), after: usageOf(150*mib, 100*mib), limitMB: 100}
	if done, _ := reconcileServer(ctx, rdb, q2, "srv"); done || q2.assigned {
		t.Fatal("a later boot walked the server again")
	}
	if n, _ := rdb.Exists(ctx, diskGraceKey("srv")).Result(); n != 0 {
		t.Fatal("a later boot granted the grace again")
	}
	forgetDiskLimit(ctx, rdb, "srv")
	if n, _ := rdb.Exists(ctx, diskCountedKey("srv")).Result(); n != 0 {
		t.Fatal("forgetting the server kept its counted marker")
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
// rather than lapsing after the marker's TTL. G47/G49: a held server whose
// container still runs is left to its collector - refreshing its marker kept
// the guard from ever stopping it again.
func TestReleaseResolvedDiskHolds(t *testing.T) {
	prevNodeID, prevMgr := nodeID, globalStorageMgr
	t.Cleanup(func() { nodeID, globalStorageMgr = prevNodeID, prevMgr })
	nodeID = "node-1"
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		limitMB  int64
		size     int
		running  bool
		released bool
		refresh  bool
	}{
		{"limit removed", 0, 4096, false, true, false},
		{"still over", 1, 2 << 20, false, false, true},
		{"back under", 1, 4096, false, true, false},
		{"still over and running", 1, 2 << 20, true, false, false},
		{"back under and running", 1, 4096, true, false, false},
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

			releaseResolvedDiskHolds(ctx, rdb, NewQuotaSet(nil), map[string]bool{held: tc.running})

			if released := !mr.Exists(diskFullKey(held)); released != tc.released {
				t.Fatalf("released = %v, want %v", released, tc.released)
			}
			if !tc.released {
				if refreshed := mr.TTL(diskFullKey(held)) > time.Minute; refreshed != tc.refresh {
					t.Fatalf("hold refreshed = %v (TTL %v), want %v", refreshed, mr.TTL(diskFullKey(held)), tc.refresh)
				}
			} else if got, _ := mr.Get(fmt.Sprintf("dylaris:server:%s:status", held)); got != "stopped" {
				t.Fatalf("status = %q, want stopped", got)
			}
		})
	}
}

// G37: a lookup of the server being moved resolved to the source during the
// copy, so a write made then landed in the tree the move deletes. It has to
// wait for the move, as it did under the old manager-wide lock, while the
// sweeps over every server must not wait on it.
func TestALookupOfTheMovingServerWaitsForTheMove(t *testing.T) {
	sm, _ := newPlacementManager(t, 2)
	paths := sm.Paths()
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	globalStorageMgr = sm
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7"
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

	resolved := make(chan string, 1)
	go func() { resolved <- sm.GetServerDir(uuid) }()
	select {
	case dir := <-resolved:
		close(release)
		t.Fatalf("the moving server resolved to %q during the copy", dir)
	case <-time.After(200 * time.Millisecond):
	}

	listed := make(chan struct{})
	go func() { localServerUUIDs(); close(listed) }()
	select {
	case <-listed:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the sweep over every server waited on the move")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("MigrateServerPath: %v", err)
	}
	if dir := <-resolved; dir != filepath.Join(paths[1], uuid) {
		t.Fatalf("resolved %q after the move, want the target", dir)
	}
}
