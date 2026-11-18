package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// R25-05: a node killed after the swap and before carryArchivesAcrossSwap left
// the archive's node files in the restored root and the live ones in the stash.
// The boot repair only filled in what the root lacked, so the reconciler rebuilt
// the container from the archive's limits. A node file in the root that is
// older than the swap is the archive's and gives way to the live one; one
// written after the swap is the node's own and stays.
func TestRestoreCleanupPutsTheLiveNodeFilesBackOverTheArchives(t *testing.T) {
	stamp := func(at time.Time) string { return at.UTC().Format("20060102-150405") }
	tests := []struct {
		name      string
		stashedAt time.Time
		writtenAt time.Time // the target's copies
		wantLive  bool
		wantKept  bool
	}{
		{"fresh", time.Now().Add(-time.Hour), time.Now().Add(-2 * time.Hour), true, false},
		{"past the TTL", time.Now().Add(-48 * time.Hour), time.Now().Add(-49 * time.Hour), true, false},
		{"rewritten after the swap", time.Now().Add(-48 * time.Hour), time.Now(), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm, _ := newPlacementManager(t, 1)
			root := sm.Paths()[0]
			const uuid = "srv-r2505"
			target := filepath.Join(root, uuid)
			stash := target + ".pre-restore-" + stamp(tc.stashedAt)
			names := []string{".node_config.json", ".active_server", ".dylaris.json"}
			for _, dir := range []string{stash, target} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, n := range names {
				if err := os.WriteFile(filepath.Join(stash, n), []byte("live"), 0o644); err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(target, n)
				if err := os.WriteFile(p, []byte("archive"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, tc.writtenAt, tc.writtenAt); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(stash, tc.stashedAt, tc.stashedAt); err != nil {
				t.Fatal(err)
			}

			runRestoreCleanup(sm)

			want := "archive"
			if tc.wantLive {
				want = "live"
			}
			for _, n := range names {
				if b, err := os.ReadFile(filepath.Join(target, n)); err != nil || string(b) != want {
					t.Errorf("%s in the root = %q, %v; want %q", n, b, err, want)
				}
			}
			kept, _ := filepath.Glob(filepath.Join(root, ".restore-kept-*"))
			if (len(kept) > 0) != tc.wantKept {
				t.Errorf("kept stashes %v, want kept = %v", kept, tc.wantKept)
			}
		})
	}
}
