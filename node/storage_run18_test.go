package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const run18UUID = "adadadad-1111-2222-3333-444444444444"

// Y08: a move of a server keyed to a configured path that was skipped at boot
// took the leftover copy a killed move left on another path as its source,
// copied the half-deleted tree to the target and re-keyed the server to it.
func TestAMoveOfAServerOnAnUnmountedPathIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(sm *StorageManager, target string) error
	}{
		{"check", func(sm *StorageManager, target string) error { return sm.CheckMigration(run18UUID, target) }},
		{"move", func(sm *StorageManager, target string) error { return sm.MigrateServerPath(run18UUID, target) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			leftover, target := t.TempDir(), t.TempDir()
			missing := filepath.Join(t.TempDir(), "unmounted")
			sm := NewStorageManager(leftover+","+target+","+missing, rdb)
			if len(sm.Paths()) != 2 {
				t.Fatalf("paths = %v, want the two mounted ones", sm.Paths())
			}
			if err := os.MkdirAll(filepath.Join(leftover, run18UUID), 0o755); err != nil {
				t.Fatal(err)
			}
			key := "node:" + nodeID + ":server:" + run18UUID + ":storage"
			mr.Set(key, missing)
			err := tc.run(sm, target)
			if err == nil || !strings.Contains(err.Error(), "not mounted") {
				t.Fatalf("err = %v, want the unmounted refusal", err)
			}
			if got, _ := mr.Get(key); got != missing {
				t.Errorf("storage key = %q, want it left on %q", got, missing)
			}
			if _, err := os.Stat(filepath.Join(leftover, run18UUID)); err != nil {
				t.Errorf("the leftover copy was touched: %v", err)
			}
		})
	}
}

// Y09: a storage-key GET Redis could not answer read as "no key", and the
// scanned copy was written back as the key for good. A key holding the wrong
// type fails the GET and lets a SET through, as a blip that ends does.
func TestAnUnreadableStorageKeyIsNeverOverwrittenByAScan(t *testing.T) {
	for _, tc := range []struct {
		name        string
		skippedPath bool
		wantCopy    bool
	}{
		{"no path skipped", false, true},
		{"a configured path skipped", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			live := t.TempDir()
			csv := live
			if tc.skippedPath {
				csv += "," + filepath.Join(t.TempDir(), "unmounted")
			}
			sm := NewStorageManager(csv, rdb)
			if err := os.MkdirAll(filepath.Join(live, run18UUID), 0o755); err != nil {
				t.Fatal(err)
			}
			key := "node:" + nodeID + ":server:" + run18UUID + ":storage"
			if _, err := mr.Lpush(key, "x"); err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.wantCopy {
				want = live
			}
			if got := sm.GetServerPath(run18UUID); got != want {
				t.Fatalf("GetServerPath = %q, want %q", got, want)
			}
			if typ := mr.Type(key); typ != "list" {
				t.Fatalf("storage key is now a %s, the scanned guess was written back", typ)
			}
			if _, err := sm.resolveMigration(run18UUID, live); err == nil || !strings.Contains(err.Error(), "storage key") {
				t.Fatalf("resolveMigration err = %v, want the unreadable key refused", err)
			}
		})
	}
}

// Y10: skipped configured paths reach the heartbeat, so Core does not list
// their servers as missing from disk.
func TestSkippedPathsAreReported(t *testing.T) {
	live := t.TempDir()
	missing := filepath.Join(t.TempDir(), "unmounted")
	if got := NewStorageManager(live, nil).SkippedPaths(); len(got) != 0 {
		t.Fatalf("SkippedPaths = %v, want none", got)
	}
	if got := NewStorageManager(live+","+missing, nil).SkippedPaths(); fmt.Sprint(got) != fmt.Sprint([]string{missing}) {
		t.Fatalf("SkippedPaths = %v, want [%s]", got, missing)
	}
}

// Y11: release 2026.11.17.2 wrote a marker without a number onto a bare
// mountpoint, and it still vouched for the path beside numbered markers.
func TestAGenlessMarkerListingPathsDoesNotVouchBesideNumberedOnes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		disk1, mnt string // marker bodies; "%s" is the csv's lines
		wantKept   bool
	}{
		{"old marker beside numbered ones", "%s\n#gen 5\n", "%s\n", false},
		{"empty override beside numbered ones", "%s\n#gen 5\n", "", true},
		{"all old markers (first boot of a numbering node)", "%s\n", "%s\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk1, mnt := t.TempDir(), t.TempDir()
			lines := disk1 + "\n" + mnt
			body := func(f string) []byte {
				if strings.Contains(f, "%s") {
					return []byte(fmt.Sprintf(f, lines))
				}
				return []byte(f)
			}
			if err := os.WriteFile(filepath.Join(disk1, storageMarkerName), body(tc.disk1), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mnt, storageMarkerName), body(tc.mnt), 0o644); err != nil {
				t.Fatal(err)
			}
			sm := NewStorageManager(disk1+","+mnt, nil)
			kept := false
			for _, p := range sm.Paths() {
				kept = kept || p == mnt
			}
			if kept != tc.wantKept {
				t.Fatalf("paths = %v, want the mountpoint kept=%v", sm.Paths(), tc.wantKept)
			}
		})
	}
}

// Y12: a Valkey restart while the node runs lost the cached limit and the
// grace, and the project's ceiling read as the server's limit until the next
// boot. The tick repairs both from the saved config.
func TestATickAfterRedisLostItRepairsTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     *DiskUsagePayload
		wantGrace bool
		wantSet   []int64
		wantLimit int64
	}{
		{"graced and still over", usageOf(150*mib, 250*mib), true, nil, 100 * mib},
		{"grace over and the ceiling still on", usageOf(50*mib, 250*mib), false, []int64{100}, 100 * mib},
		{"never graced", usageOf(10*mib, 100*mib), false, nil, 100 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalStorageMgr
			t.Cleanup(func() { globalStorageMgr = prev })
			rdb := newDiskGuardRedis(t)
			root := t.TempDir()
			globalStorageMgr = NewStorageManager(root, rdb)
			ctx := context.Background()
			dir := filepath.Join(root, run18UUID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			saveNodeConfigWithDiskLimit(dir, ServerConfig{UUID: run18UUID, Docker: DockerConfig{DiskLimit: 100}})
			if err := rdb.FlushAll(ctx).Err(); err != nil {
				t.Fatal(err)
			}
			q := &fakeQuota{limitMB: tc.usage.Limit >> 20}
			repairLostDiskLimit(ctx, rdb, q, run18UUID, tc.usage)
			withGraceLimit(ctx, rdb, run18UUID, tc.usage)
			if _, ok := loadDiskGrace(ctx, rdb, run18UUID); ok != tc.wantGrace {
				t.Fatalf("grace = %v, want %v", ok, tc.wantGrace)
			}
			if fmt.Sprint(q.setLimits) != fmt.Sprint(tc.wantSet) {
				t.Fatalf("limits set %v, want %v", q.setLimits, tc.wantSet)
			}
			if tc.usage.Limit != tc.wantLimit || loadDiskLimit(ctx, rdb, run18UUID) != 100 {
				t.Fatalf("reported limit %d, cached %d MB, want %d and 100", tc.usage.Limit, loadDiskLimit(ctx, rdb, run18UUID), tc.wantLimit)
			}
		})
	}
}
