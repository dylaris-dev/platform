package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"dylaris-pkg/migration"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// movedAwayLeftovers are the renamed copies removeCopy parks a server under.
func movedAwayLeftovers(t *testing.T, path, uuid string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(path, "."+uuid+parkedCopyInfix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// R32-10: a copy whose moved-away mark could not be written (ENOSPC on a full
// XFS disk, EDQUOT at the project limit) was never deleted, so a move off a
// full disk never freed it. A rename needs no blocks: the copy leaves its
// <uuid> name and then goes.
func TestRemoveCopyWithoutRoomForTheMark(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deleteFail bool
	}{
		{"delete succeeds", false},
		{"delete fails partway", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
			paths := sm.Paths()
			dir := writeCopy(t, paths[0], run18UUID, time.Now())
			writeCopy(t, paths[1], run18UUID, time.Now())
			// A directory in the mark's place fails the write like a full disk.
			if err := os.Mkdir(filepath.Join(dir, storageMovedAwayMarker), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.deleteFail {
				prev := removeTree
				t.Cleanup(func() { removeTree = prev })
				removeTree = func(string) error { return errors.New("input/output error") }
			}
			err := removeCopy(dir)
			if tc.deleteFail != (err != nil) {
				t.Fatalf("removeCopy = %v", err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the copy still holds its server name: %v", err)
			}
			if got := sm.PathsHolding(run18UUID); len(got) != 1 || got[0] != paths[1] {
				t.Fatalf("PathsHolding = %v", got)
			}
			left := movedAwayLeftovers(t, paths[0], run18UUID)
			if tc.deleteFail != (len(left) == 1) {
				t.Fatalf("renamed leftovers = %v", left)
			}
			if !tc.deleteFail {
				return
			}
			// The next node start frees what the failed delete left.
			removeTree = os.RemoveAll
			sweepInterruptedWork(paths)
			if left := movedAwayLeftovers(t, paths[0], run18UUID); len(left) != 0 {
				t.Fatalf("leftovers after the boot sweep = %v", left)
			}
			if got := sm.PathsHolding(run18UUID); len(got) != 1 {
				t.Fatalf("the boot sweep touched the live copy: %v", got)
			}
		})
	}
}

// R32-11 / R32-09: a move killed during the copy, or one whose key write could
// not be confirmed, left a target that the markers named live (the copy
// brought the source's mark along, and the new one is higher). After a key
// loss that tied with the source, or won with data the server never ran on.
// The target stays marked moved away until its key is written.
func TestAnUnfinishedMoveTargetNeverWinsAfterAKeyLoss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		srcLive string
		during  func(mr *miniredis.Miniredis) // inside the copy
		wantErr error
	}{
		// The kill is the copy hook's state: everything copied, nothing after.
		{"killed during the copy, first move", "", nil, nil},
		{"killed during the copy, moved before", "100", nil, nil},
		{"key write unconfirmed", "100", func(mr *miniredis.Miniredis) { mr.SetError("LOADING") }, errStorageKeyUnsure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
			paths := sm.Paths()
			src := writeCopy(t, paths[0], run18UUID, time.Now().Add(-time.Hour))
			if tc.srcLive != "" {
				if err := os.WriteFile(filepath.Join(src, storageLiveMarker), []byte(tc.srcLive), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := sm.setRedisPath(run18UUID, paths[0]); err != nil {
				t.Fatal(err)
			}
			prev := migrateCopyTree
			t.Cleanup(func() { migrateCopyTree = prev })
			var killed string
			migrateCopyTree = func(s, d string) error {
				if err := prev(s, d); err != nil {
					return err
				}
				if tc.during != nil {
					tc.during(mr)
					return nil
				}
				killed = markedLiveCopy(run18UUID, sm.PathsHolding(run18UUID))
				return errors.New("killed")
			}
			err := sm.MigrateServerPath(run18UUID, paths[1])
			if tc.during == nil {
				if killed != paths[0] {
					t.Fatalf("killed during the copy, the markers name %q live, want the source %q", killed, paths[0])
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("MigrateServerPath = %v, want %v", err, tc.wantErr)
			}
			mr.SetError("")
			mr.FlushAll()
			// The key could not be read back, so it may name either copy and
			// the server may have run on the target since: neither wins
			// (R33-08/R33-09), the server is reported ambiguous instead.
			if got := sm.GetServerPath(run18UUID); got != "" {
				t.Fatalf("after the key loss GetServerPath = %q, want unresolved", got)
			}
			if amb := sm.StorageAmbiguities(); len(amb) != 1 {
				t.Fatalf("StorageAmbiguities = %v, want the server", amb)
			}
		})
	}
}

// A finished move leaves the target live and unmarked, and the source gone.
func TestAFinishedMoveLeavesTheTargetUnmarked(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
	paths := sm.Paths()
	writeCopy(t, paths[0], run18UUID, time.Now())
	if err := sm.setRedisPath(run18UUID, paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := sm.MigrateServerPath(run18UUID, paths[1]); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(paths[1], run18UUID)
	if _, err := os.Stat(filepath.Join(dst, storageMovedAwayMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the live target is marked moved away: %v", err)
	}
	if _, ok, err := liveGeneration(dst); !ok || err != nil {
		t.Fatalf("the target carries no live mark: %v", err)
	}
	if got := sm.PathsHolding(run18UUID); len(got) != 1 || got[0] != paths[1] {
		t.Fatalf("PathsHolding = %v", got)
	}
}

// R32-12: a moved-in copy arrived with the source node's markers, a
// generation from another machine's clock, so a leftover copy here could tie
// with it or outrank it after a key loss. It lands with its own live mark,
// above every other copy on this node.
func TestAMovedInCopyIsMarkedLiveAboveTheLocalCopies(t *testing.T) {
	storage := t.TempDir()
	other := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	for name, data := range map[string]string{
		storageLiveMarker:      "5",
		storageMovedAwayMarker: "5",
		"server.properties":    "x",
	} {
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	zipPath := filepath.Join(t.TempDir(), "a.zip")
	if _, _, err := migration.Archive(src, zipPath); err != nil {
		t.Fatal(err)
	}
	// A leftover on another path of this node whose mark is far ahead of now.
	leftover := writeCopy(t, other, run18UUID, time.Now())
	far := time.Now().Add(time.Hour).UnixNano()
	if err := os.WriteFile(filepath.Join(leftover, storageLiveMarker), []byte(strconv.FormatInt(far, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(storage, run18UUID)
	if err := extractMovedServer(zipPath, storage, dir, nil, []string{storage, other}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, storageMovedAwayMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the archived moved-away mark arrived: %v", err)
	}
	gen, ok, err := liveGeneration(dir)
	if !ok || err != nil || gen <= far {
		t.Fatalf("live mark = %d (ok=%v, %v), want above the leftover's %d", gen, ok, err, far)
	}
	if got := markedLiveCopy(run18UUID, []string{other, storage}); got != storage {
		t.Fatalf("markedLiveCopy = %q, want the moved-in copy", got)
	}
}
