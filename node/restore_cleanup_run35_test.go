package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// R35-08: a node killed during the restore's delete of the old world left the
// rest of it as a stash beside the restored root, under the server's quota
// project, for the 24h TTL. A whole-server stash whose swap and carry are done
// is what the restore was deleting, and goes on the next sweep. A sub-server
// stash keeps its TTL: its name sits inside the server, where a tenant can
// create one.
func TestRestoreCleanupFinishesTheDeleteOfAnInterruptedRestore(t *testing.T) {
	stamp := time.Now().UTC().Format("20060102-150405")
	tests := []struct {
		name     string
		target   string // relative to the root
		running  bool
		wantGone bool
	}{
		{"whole server, swap and carry done", "srv-r35", false, true},
		{"a restore of it is running", "srv-r35", true, false},
		{"sub-server", filepath.Join("srv-r35", "survival"), false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm, _ := newPlacementManager(t, 1)
			root := sm.Paths()[0]
			target := filepath.Join(root, tc.target)
			stash := target + ".pre-restore-" + stamp
			for p, body := range map[string]string{
				filepath.Join(stash, "world", "r.0.0.mca"): "rest of the old world",
				filepath.Join(target, "level.dat"):         "restored world",
			} {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.running {
				uuid := strings.SplitN(filepath.ToSlash(tc.target), "/", 2)[0]
				restoreServersInFlight.enter(uuid)
				defer restoreServersInFlight.leave(uuid)
			}

			runRestoreCleanup(sm)

			_, err := os.Stat(stash)
			if gone := os.IsNotExist(err); gone != tc.wantGone {
				t.Fatalf("stash gone = %v, want %v (stat err %v)", gone, tc.wantGone, err)
			}
			if b, err := os.ReadFile(filepath.Join(target, "level.dat")); err != nil || string(b) != "restored world" {
				t.Fatalf("restored root touched: %q, %v", b, err)
			}
		})
	}
}
