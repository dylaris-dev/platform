package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R31-08 / R31-09: two copies with no storage key were settled by the newest
// mtime, and a move's own debris is written last: the delete of a source bumps
// its directories' mtimes, and a copy killed halfway is newer than its source.
// The key was then written to the debris and the next move deleted the real
// copy. Only the node's own markers settle it now: a copy marked moved away
// never wins, and the newest live mark wins among the rest. Copies an older
// node left carry neither and stay ambiguous, whatever their mtimes say.
func TestTwoCopiesWithoutAKeyResolveByTheMoveMarkers(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	newer := old.Add(time.Hour)
	type copyState struct {
		at    time.Time
		live  string // .dylaris_live content, "" for none
		moved bool   // .dylaris_moved_away present
	}
	for _, tc := range []struct {
		name   string
		copies [2]copyState
		want   int // index into the paths, -1 for ""
	}{
		// R31-08: the source of a finished move, half deleted after the key
		// flip, is newer than the target nothing has written since.
		{"half-deleted source is newer", [2]copyState{{at: newer, live: "100", moved: true}, {at: old, live: "200"}}, 1},
		// R31-09: a copy killed halfway is newer than the complete source.
		{"partial target is newer", [2]copyState{{at: old, live: "100"}, {at: newer}}, 0},
		{"newest live mark wins", [2]copyState{{at: newer, live: "100"}, {at: old, live: "200"}}, 1},
		{"one not moved away", [2]copyState{{at: newer, moved: true}, {at: old}}, 1},
		{"both moved away", [2]copyState{{at: old, moved: true}, {at: newer, moved: true}}, -1},
		{"equal live marks", [2]copyState{{at: old, live: "100"}, {at: newer, live: "100"}}, -1},
		{"left by an old node, mtimes differ", [2]copyState{{at: old}, {at: newer}}, -1},
		{"unreadable live mark", [2]copyState{{at: old, live: "x"}, {at: newer}}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
			paths := sm.Paths()
			var dirs []string
			for i, c := range tc.copies {
				d := writeCopy(t, paths[i], run18UUID, c.at)
				if c.live != "" {
					if err := os.WriteFile(filepath.Join(d, storageLiveMarker), []byte(c.live), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if c.moved {
					if err := os.WriteFile(filepath.Join(d, storageMovedAwayMarker), []byte("1"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				dirs = append(dirs, d)
			}
			want := ""
			if tc.want >= 0 {
				want = paths[tc.want]
			}
			if got := sm.GetServerPath(run18UUID); got != want {
				t.Fatalf("GetServerPath = %q, want %q", got, want)
			}
			if v, _ := mr.Get("node:" + nodeID + ":server:" + run18UUID + ":storage"); v != want {
				t.Fatalf("storage key = %q, want %q", v, want)
			}
			for _, d := range dirs {
				if _, err := os.Stat(d); err != nil {
					t.Fatalf("copy %s gone after the lookup: %v", d, err)
				}
			}
			amb := sm.StorageAmbiguities()
			if wantAmb := tc.want < 0; (len(amb) == 1) != wantAmb {
				t.Fatalf("ambiguities = %+v, want reported=%v", amb, wantAmb)
			}
			if tc.want < 0 {
				// Settled by hand: the stale tree removed, the report goes.
				if err := os.RemoveAll(dirs[0]); err != nil {
					t.Fatal(err)
				}
				if got := sm.GetServerPath(run18UUID); got != paths[1] {
					t.Fatalf("after removing one copy: GetServerPath = %q", got)
				}
				if amb := sm.StorageAmbiguities(); len(amb) != 0 {
					t.Fatalf("ambiguities after the fix = %+v", amb)
				}
			}
		})
	}
}

// R31-08 end to end: the source delete of a move fails partway, as on the
// failing disk being moved off, then Valkey restarts. The gutted source is
// the newest tree on disk, and it must not become the server.
func TestAHalfDeletedMoveSourceNeverWinsAfterAKeyLoss(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
	paths := sm.Paths()
	writeCopy(t, paths[0], run18UUID, time.Now().Add(-time.Hour))
	if err := sm.setRedisPath(run18UUID, paths[0]); err != nil {
		t.Fatal(err)
	}

	prev := removeTree
	t.Cleanup(func() { removeTree = prev })
	removeTree = func(dir string) error {
		// The source, parked under its hidden name by removeCopy.
		if filepath.Dir(dir) != paths[0] || !strings.HasPrefix(filepath.Base(dir), "."+run18UUID+parkedCopyInfix) {
			return prev(dir)
		}
		// Most entries go, the root and its marker stay, and the directory
		// mtimes are now the newest on either path.
		if err := os.RemoveAll(filepath.Join(dir, "survival")); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(dir, ".node_config.json")); err != nil {
			return err
		}
		return errors.New("input/output error")
	}
	if err := sm.MigrateServerPath(run18UUID, paths[1]); err != nil {
		t.Fatalf("MigrateServerPath: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths[1], run18UUID, storageLiveMarker)); err != nil {
		t.Fatalf("the target carries no live mark: %v", err)
	}

	mr.FlushAll()
	if got := sm.GetServerPath(run18UUID); got != paths[1] {
		t.Fatalf("after the key loss GetServerPath = %q, want the complete copy %q", got, paths[1])
	}
	if _, err := os.Stat(filepath.Join(paths[1], run18UUID, "survival", "usercache.json")); err != nil {
		t.Fatalf("the complete copy lost its files: %v", err)
	}
}

// Both marker names are the node's: a tenant who could write one could pick
// which copy the node keeps after a key loss, and so which one it deletes.
func TestTheMoveMarkersAreProtected(t *testing.T) {
	for _, name := range []string{storageLiveMarker, storageMovedAwayMarker} {
		if !isProtectedFile(name) {
			t.Errorf("isProtectedFile(%q) = false", name)
		}
	}
}
