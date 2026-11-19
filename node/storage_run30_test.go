package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// writeCopy makes a server copy on root whose node-written files and
// sub-server entries all carry mtime at.
func writeCopy(t *testing.T, root, uuid string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(root, uuid)
	files := []string{".node_config.json", filepath.Join("survival", "usercache.json")}
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(dir, files[0]), filepath.Join(dir, files[1]), filepath.Join(dir, "survival")} {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// R30-09: since R29-07, two copies with no storage key (every key is gone
// after a Valkey restart) resolved to neither for good, and nothing wrote the
// key again. The copy the node last wrote is the live one: a move writes its
// target after the source, and a server runs only on the copy its key named.
// Only copies that cannot be told apart stay unresolved, and are reported.
func TestTwoCopiesWithoutAKeyResolveToTheLastWrittenOne(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	newer := old.Add(time.Hour)
	for _, tc := range []struct {
		name  string
		times [2]time.Time
		world int // copy whose usercache.json is written after everything, -1 for none
		want  int // index into the paths, -1 for ""
	}{
		{"leftover on the first path", [2]time.Time{old, newer}, -1, 1},
		{"leftover on the second path", [2]time.Time{newer, old}, -1, 0},
		{"same config but the world written later on one", [2]time.Time{old, old}, 0, 0},
		{"indistinguishable", [2]time.Time{old, old}, -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			a, b := t.TempDir(), t.TempDir()
			sm := NewStorageManager(a+","+b, rdb)
			paths := sm.Paths()
			dirs := []string{writeCopy(t, paths[0], run18UUID, tc.times[0]), writeCopy(t, paths[1], run18UUID, tc.times[1])}
			if tc.world >= 0 {
				p := filepath.Join(dirs[tc.world], "survival", "usercache.json")
				if err := os.Chtimes(p, newer.Add(time.Hour), newer.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			want := ""
			if tc.want >= 0 {
				want = paths[tc.want]
			}
			if got := sm.GetServerPath(run18UUID); got != want {
				t.Fatalf("GetServerPath = %q, want %q", got, want)
			}
			key := "node:" + nodeID + ":server:" + run18UUID + ":storage"
			if v, _ := mr.Get(key); v != want {
				t.Fatalf("storage key = %q, want %q", v, want)
			}
			// The other copy is never removed by a lookup.
			for _, d := range dirs {
				if _, err := os.Stat(d); err != nil {
					t.Fatalf("copy %s gone after the lookup: %v", d, err)
				}
			}
			amb := sm.StorageAmbiguities()
			if wantAmb := tc.want < 0; (len(amb) == 1) != wantAmb || (wantAmb && (amb[0].Server != run18UUID || len(amb[0].Paths) != 2)) {
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

// R30-08: a delete of a server with two copies removed at most the one the
// lookup resolved, and with no key nothing at all, while the orphan delete in
// the panel reported success. A deleted server has no live copy to protect.
func TestADeleteRemovesEveryCopy(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	for _, c := range []struct {
		name  string
		times [2]time.Time
		key   int // index of the path the key names, -1 for none
	}{
		{"indistinguishable and no key", [2]time.Time{old, old}, -1},
		{"distinguishable and no key", [2]time.Time{old, old.Add(time.Hour)}, -1},
		{"key names one", [2]time.Time{old, old}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			const uuid = sharedDaemonUUID
			t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
			ctx := context.Background()
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			dm, _ := newSharedDaemon(t, "-", false, nil)
			storage := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
			dm.storageMgr = storage
			paths := storage.Paths()
			dirs := []string{writeCopy(t, paths[0], uuid, c.times[0]), writeCopy(t, paths[1], uuid, c.times[1])}
			storageKey := fmt.Sprintf("node:%s:server:%s:storage", nodeID, uuid)
			if c.key >= 0 {
				rdb.Set(ctx, storageKey, paths[c.key], 0)
			}
			recordDiskLimit(ctx, rdb, uuid, 100)

			cmd := NodeCommand{Action: "delete"}
			cmd.Config.UUID = uuid
			processCommand(ctx, cmd, "", rdb, dm, "node-a", nil, storage)

			for _, d := range dirs {
				if _, err := os.Stat(d); !os.IsNotExist(err) {
					t.Errorf("copy %s still on disk (err %v)", d, err)
				}
			}
			for _, key := range []string{storageKey, diskLimitKey(uuid)} {
				if rdb.Exists(ctx, key).Val() == 1 {
					t.Errorf("%s kept after a full delete", key)
				}
			}
			if amb := storage.StorageAmbiguities(); len(amb) != 0 {
				t.Errorf("ambiguities after the delete = %+v", amb)
			}
		})
	}
}

// R30-07: a path whose probe always needs longer than storageProbeTimeout but
// less than a beat read unavailable with zero capacity on every second beat:
// the beat after a served probe started a new one and waited only the window.
// The last finished result stands in while the next probe runs, until that
// probe has run longer than storageProbeStale.
func TestASlowStorageProbeKeepsItsLastResult(t *testing.T) {
	slow := t.TempDir()
	sm := &StorageManager{paths: []string{slow}}
	var calls atomic.Int32
	gate := make(chan struct{}, 10)
	prev, prevTimeout, prevStale := diskSpaceOf, storageProbeTimeout, storageProbeStale
	t.Cleanup(func() { diskSpaceOf, storageProbeTimeout, storageProbeStale = prev, prevTimeout, prevStale })
	storageProbeTimeout = 50 * time.Millisecond
	storageProbeStale = time.Hour
	diskSpaceOf = func(p string) (uint64, uint64) {
		n := calls.Add(1)
		<-gate
		return uint64(n) << 30, 1 << 29
	}
	finish := func() {
		gate <- struct{}{}
		sm.probeMu.Lock()
		pr := sm.probes[slow]
		sm.probeMu.Unlock()
		<-pr.done
	}
	for _, tc := range []struct {
		beat       int
		unreadable bool
		total      uint64 // GiB
		finish     bool   // let the running probe finish after the beat
	}{
		{1, true, 0, true},   // nothing finished yet
		{2, false, 1, false}, // the late result
		{3, false, 1, true},  // a new probe runs: the last result stands in
		{4, false, 2, false}, // the new probe's own result
	} {
		infos := sm.GetStorageInfo()
		if len(infos) != 1 || infos[0].unreadable != tc.unreadable {
			t.Fatalf("beat %d: infos = %+v, want unreadable=%v", tc.beat, infos, tc.unreadable)
		}
		if infos[0].TotalBytes != tc.total<<30 {
			t.Fatalf("beat %d: TotalBytes = %d, want %d GiB", tc.beat, infos[0].TotalBytes, tc.total)
		}
		if tc.finish {
			finish()
		}
	}

	// A probe running past storageProbeStale is a hung path, not a slow one.
	storageProbeStale = 0
	infos := sm.GetStorageInfo() // starts probe 3, which never finishes here
	if len(infos) != 1 || !infos[0].unreadable || infos[0].TotalBytes != 0 {
		t.Fatalf("hung probe: infos = %+v, want unreadable with no capacity", infos)
	}
	finish()
}
