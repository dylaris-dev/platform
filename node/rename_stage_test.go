package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// renameNoFollow parks what it moves under an intermediate name between its two
// renameat calls. That name used to be a ".pending-delete-" tombstone, so a
// folder left there by a node restart was RemoveAll'd by the first sweep. It
// must stay refused to every write path, and never be swept.
func TestRenameIntermediateIsProtectedButNeverSwept(t *testing.T) {
	const parked = ".dylaris-mv-0123456789abcdef"
	for _, p := range []string{parked, "survival/" + parked, parked + "/level.dat"} {
		if !isProtectedFile(p) {
			t.Errorf("isProtectedFile(%q) = false: a write path could reach the parked entry", p)
		}
	}

	sm, _ := newPlacementManager(t, 1)
	root := sm.Paths()[0]
	old := time.Now().Add(-48 * time.Hour)
	var kept []string
	for _, rel := range []string{filepath.Join("srv-uuid", parked), filepath.Join("srv-uuid", "survival", parked)} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, old, old); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, full)
	}
	runRestoreCleanup(sm)
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("sweep deleted a folder caught mid-rename: %s (%v)", p, err)
		}
	}
}
