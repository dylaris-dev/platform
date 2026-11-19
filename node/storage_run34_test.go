package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R34-10: with no storage key (a Valkey restart) and a storage path skipped at
// boot, the one copy the scan found was written back as the key, also when it
// was a move's half-deleted source. The real copy, waiting on the unmounted
// disk, then read as the leftover for good. With a path skipped the scan is
// never written back, and a copy marked moved away is no candidate.
func TestASingleCopyIsNotKeyedWhileAPathIsSkipped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		skipped   bool
		movedAway bool
		want      bool // resolves to the copy
		wantKey   bool
	}{
		{"all mounted unmarked", false, false, true, true},
		{"all mounted marked moved away is the only data", false, true, true, true},
		{"path skipped unmarked", true, false, true, false},
		{"path skipped marked moved away", true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			live := t.TempDir()
			csv := live
			if tc.skipped {
				csv += "," + filepath.Join(t.TempDir(), "unmounted")
			}
			sm := NewStorageManager(csv, rdb)
			dir := writeCopy(t, live, run18UUID, time.Now())
			if tc.movedAway {
				if err := os.WriteFile(filepath.Join(dir, storageMovedAwayMarker), []byte("1"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want := ""
			if tc.want {
				want = live
			}
			if got := sm.GetServerPath(run18UUID); got != want {
				t.Fatalf("GetServerPath = %q, want %q", got, want)
			}
			key, _ := mr.Get("node:" + nodeID + ":server:" + run18UUID + ":storage")
			if (key != "") != tc.wantKey {
				t.Fatalf("storage key = %q, want written = %v", key, tc.wantKey)
			}
		})
	}
}

// R34-12: removeCopy marked a copy moved away and then deleted it with
// RemoveAll, which unlinks in readdir order, the mark included. A delete
// stopped after the mark went left an unmarked partial copy whose higher live
// mark outranked the whole one after a key loss. The copy now leaves its
// <uuid> name before anything of it is deleted.
func TestAHalfDeletedCopyNeverOutranksTheWholeOne(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
	paths := sm.Paths()
	whole := writeCopy(t, paths[0], run18UUID, time.Now())
	partial := writeCopy(t, paths[1], run18UUID, time.Now())
	for dir, gen := range map[string]string{whole: "100", partial: "200"} {
		if err := os.WriteFile(filepath.Join(dir, storageLiveMarker), []byte(gen), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(partial, storageMovedAwayMarker), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	prev := removeTree
	t.Cleanup(func() { removeTree = prev })
	removeTree = func(dir string) error {
		// Killed right after RemoveAll unlinked the mark.
		_ = os.Remove(filepath.Join(dir, storageMovedAwayMarker))
		return errors.New("killed")
	}
	if err := removeCopy(partial); err == nil {
		t.Fatal("removeCopy = nil, want the delete's error")
	}

	if got := sm.GetServerPath(run18UUID); got != paths[0] {
		t.Fatalf("after the key loss GetServerPath = %q, want the whole copy %q", got, paths[0])
	}
}
