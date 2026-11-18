package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The cleanup walk deletes directories it recognises as leftovers. It matched
// ".pre-restore-" with strings.Contains and applied no depth bound to a match,
// while the walk itself visits three levels deep - so any directory a user put
// inside a sub-server with that substring in its name was silently deleted 24
// hours later. Neither marker is reserved on the way in: ".pending-delete-" is
// only guarded against the file API, not against zip extraction.
func TestRestoreCleanupOnlyRemovesItsOwnLeftovers(t *testing.T) {
	sm, _ := newPlacementManager(t, 1)
	root := sm.Paths()[0]
	const uuid = "srv-uuid"

	old := time.Now().Add(-48 * time.Hour)
	mk := func(rel string, age time.Time) string {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, age, age); err != nil {
			t.Fatal(err)
		}
		return full
	}

	// Real leftovers: a whole-server stash (depth 1), a sub-server stash and a
	// delete tombstone (depth 2).
	gone := []string{
		mk(uuid+".pre-restore-20260806-120000", old),
		mk(filepath.Join(uuid, "survival.pre-restore-20260806-120000"), old),
		mk(filepath.Join(uuid, ".pending-delete-survival-1754500000"), old),
	}

	// User data that must survive. The sub-server itself has to be created
	// before its children so its own mtime does not matter to the walk.
	mk(filepath.Join(uuid, "survival"), time.Now())
	stays := []string{
		// Depth 3: reachable through the file browser, SFTP and zip upload.
		mk(filepath.Join(uuid, "survival", "notes.pre-restore-old"), old),
		mk(filepath.Join(uuid, "survival", "backup.pre-restore-20260806-120000"), old),
		mk(filepath.Join(uuid, "survival", ".pending-delete-mine"), old),
		// Depth 2 with the substring but no timestamp: the server directory is
		// the file browser's root, so a user can create this one directly.
		mk(filepath.Join(uuid, "notes.pre-restore-old"), old),
		// A genuine stash that is still inside its grace window.
		mk(filepath.Join(uuid, "survival.pre-restore-20260807-090000"), time.Now()),
	}

	runRestoreCleanup(sm)

	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale leftover kept: %s (stat err = %v)", filepath.Base(p), err)
		}
	}
	for _, p := range stays {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("deleted something that was not ours: %s (%v)", filepath.Base(p), err)
		}
	}
}

// A stash is made by renaming the server or sub-server directory, which keeps
// that directory's own mtime: an idle server's is days old. Judged by it, a
// stash a node died with mid-swap was gone 30 seconds after the restart instead
// of after the day the TTL promises.
func TestAPreRestoreStashAgesFromItsName(t *testing.T) {
	sm, _ := newPlacementManager(t, 1)
	root := sm.Paths()[0]
	old := time.Now().Add(-72 * time.Hour)
	stamp := func(at time.Time) string { return at.UTC().Format("20060102-150405") }
	tests := []struct {
		name     string
		stashed  time.Time
		wantKept bool
	}{
		{"stashed now from an idle directory", time.Now(), true},
		{"stashed two days ago", time.Now().Add(-48 * time.Hour), false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(root, "srv-"+string(rune('a'+i))+".pre-restore-"+stamp(tt.stashed))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatal(err)
			}
			runRestoreCleanup(sm)
			_, err := os.Stat(dir)
			if kept := err == nil; kept != tt.wantKept {
				t.Fatalf("kept=%v, want %v (stat err %v)", kept, tt.wantKept, err)
			}
		})
	}
}

// A node killed between the swap's two renames leaves "<target>.pre-restore-*"
// (the original world) and a dead stage, with nothing at <target>. The sweep
// removed the stage on sight and the stash a day later: both copies gone. The
// stash must be put back instead, whatever its age, unless a restore of that
// server is running here and may be inside that window right now. Without a
// stage the target was deleted after a finished swap and must not come back.
func TestRestoreCleanupPutsBackAStashWhoseTargetIsMissing(t *testing.T) {
	stamp := func(at time.Time) string { return at.UTC().Format("20060102-150405") }
	tests := []struct {
		name      string
		target    string // relative to the root
		stashedAt time.Time
		noStage   bool // the target was deleted after a finished swap
		running   bool
		wantBack  bool
	}{
		{"whole server fresh", "srv-a", time.Now(), false, false, true},
		{"sub-server fresh", filepath.Join("srv-b", "survival"), time.Now(), false, false, true},
		{"whole server past the TTL", "srv-c", time.Now().Add(-48 * time.Hour), false, false, true},
		{"a restore of it is running", "srv-d", time.Now(), false, true, false},
		{"no stage deleted after the swap", filepath.Join("srv-e", "survival"), time.Now(), true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm, _ := newPlacementManager(t, 1)
			root := sm.Paths()[0]
			target := filepath.Join(root, tc.target)
			stash := target + ".pre-restore-" + stamp(tc.stashedAt)
			stage := target + stageDirInfix + "42" + stageDirSuffix
			dirs := []string{stash, stage}
			if tc.noStage {
				dirs = dirs[:1]
			}
			for _, d := range dirs {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(stash, "level.dat"), []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			uuid := strings.SplitN(filepath.ToSlash(tc.target), "/", 2)[0]
			if tc.running {
				restoreServersInFlight.enter(uuid)
				defer restoreServersInFlight.leave(uuid)
			}

			runRestoreCleanup(sm)

			b, err := os.ReadFile(filepath.Join(target, "level.dat"))
			if back := err == nil && string(b) == "original"; back != tc.wantBack {
				t.Fatalf("original world back at its name = %v, want %v (err %v)", back, tc.wantBack, err)
			}
			if !tc.wantBack {
				if _, err := os.Stat(stash); err != nil {
					t.Fatalf("stash of a running restore was touched: %v", err)
				}
			}
		})
	}
}
