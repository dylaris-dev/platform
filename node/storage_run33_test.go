package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// flakyStorageKey fails the storage key commands of a move while on: a SET
// that is applied (or refused) and then errs, and the first getFails GETs.
type flakyStorageKey struct {
	on       bool
	applySet bool
	getFails int
}

func (*flakyStorageKey) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*flakyStorageKey) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *flakyStorageKey) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if !h.on {
			return next(ctx, cmd)
		}
		fail := errors.New("i/o timeout")
		switch cmd.Name() {
		case "set":
			if h.applySet {
				if err := next(ctx, cmd); err != nil {
					return err
				}
			}
			cmd.SetErr(fail)
			return fail
		case "get":
			if h.getFails != 0 {
				h.getFails--
				cmd.SetErr(fail)
				return fail
			}
		}
		return next(ctx, cmd)
	}
}

// R33-08 / R33-09: a move whose key write could not be confirmed left the
// target marked moved away for good, also when Redis had applied the SET and
// the server then ran there. After a key loss the stale source won. Now the
// key is read again: naming the target clears its mark, naming the source
// keeps it, and still unread both copies stay candidates, so the lookup
// reports the server ambiguous instead of picking one. Neither copy is deleted.
func TestAnUnsureMoveKeyNeverLeavesALiveTargetMarkedMovedAway(t *testing.T) {
	prevDelay := unsureKeyRereadDelay
	unsureKeyRereadDelay = 0
	t.Cleanup(func() { unsureKeyRereadDelay = prevDelay })
	for _, tc := range []struct {
		name          string
		applySet      bool
		getFails      int // -1: never answers
		wantTargetMrk bool
		wantAfterLoss int // index into paths, -1 for unresolved
	}{
		{"set applied, key names the target on a reread", true, 1, false, 1},
		{"set applied, key never readable", true, -1, false, -1},
		{"set refused, key names the source", false, 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm := NewStorageManager(t.TempDir()+","+t.TempDir(), rdb)
			paths := sm.Paths()
			src := writeCopy(t, paths[0], run18UUID, time.Now().Add(-time.Hour))
			if err := os.WriteFile(filepath.Join(src, storageLiveMarker), []byte("100"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := sm.setRedisPath(run18UUID, paths[0]); err != nil {
				t.Fatal(err)
			}
			hook := &flakyStorageKey{applySet: tc.applySet, getFails: tc.getFails}
			rdb.AddHook(hook)
			prev := migrateCopyTree
			t.Cleanup(func() { migrateCopyTree = prev })
			migrateCopyTree = func(s, d string) error {
				hook.on = true // after resolveMigration's read
				return prev(s, d)
			}

			if err := sm.MigrateServerPath(run18UUID, paths[1]); !errors.Is(err, errStorageKeyUnsure) {
				t.Fatalf("MigrateServerPath = %v, want errStorageKeyUnsure", err)
			}
			hook.on = false
			if got := sm.PathsHolding(run18UUID); len(got) != 2 {
				t.Fatalf("PathsHolding = %v, want both copies kept", got)
			}
			_, err := os.Stat(filepath.Join(paths[1], run18UUID, storageMovedAwayMarker))
			if marked := err == nil; marked != tc.wantTargetMrk {
				t.Fatalf("target marked moved away = %v, want %v", marked, tc.wantTargetMrk)
			}

			mr.FlushAll()
			want := ""
			if tc.wantAfterLoss >= 0 {
				want = paths[tc.wantAfterLoss]
			}
			if got := sm.GetServerPath(run18UUID); got != want {
				t.Fatalf("after the key loss GetServerPath = %q, want %q", got, want)
			}
			if amb := sm.StorageAmbiguities(); (len(amb) == 1) != (want == "") {
				t.Fatalf("StorageAmbiguities = %v", amb)
			}
		})
	}
}
