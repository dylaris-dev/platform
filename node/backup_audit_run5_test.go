package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// K18/K19: the size comes from the Content-Length a tenant's own endpoint
// declares, so it may only shorten the deadline, never stretch it past the
// ceiling - and a huge value must not overflow into an arbitrary duration.
func TestRestoreDeadlineIsCappedWhateverSizeIsDeclared(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want time.Duration
	}{
		{"just below the ceiling scales", restoreDeadlineMinRate * 3600 * 21, restoreDeadlineFloor + 21*time.Hour},
		{"at the ceiling", restoreDeadlineMinRate * 3600 * 22, restoreDeadlineMax},
		{"one tebibyte", 1 << 40, restoreDeadlineMax},
		{"ten terabytes", 10e12, restoreDeadlineMax},
		{"overflowing", math.MaxInt64, restoreDeadlineMax},
		{"overflow boundary", 2_400_000_000_000_000, restoreDeadlineMax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := restoreDeadline(tc.size); got != tc.want {
				t.Errorf("restoreDeadline(%d) = %v, want %v", tc.size, got, tc.want)
			}
		})
	}
}

// K22/K24: a stage of a restore this process is still running is never swept,
// however old its mtime. A day-long legitimate restore lost its stage mid-way.
// Once the restore has ended, a leftover stage is dead and goes at once.
func TestRestoreCleanupNeverSweepsARunningRestoresStage(t *testing.T) {
	sm, _ := newPlacementManager(t, 1)
	root := sm.Paths()[0]
	sub := filepath.Join(root, "srv-uuid-k22", "survival")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	running, err := createStageDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseStageDir(running) })
	ended, err := createStageDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	releaseStageDir(ended)
	old := time.Now().Add(-2 * preRestoreTTL)
	for _, d := range []string{running, ended} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}

	runRestoreCleanup(sm)

	if _, err := os.Stat(running); err != nil {
		t.Errorf("swept the stage of a restore still running: %v", err)
	}
	if _, err := os.Stat(ended); !os.IsNotExist(err) {
		t.Errorf("stage of an ended restore kept: %v", err)
	}
}

// K21: a node-local backup of a server deleted while the backup waited for its
// save flush must not bring the server's directory back.
func TestNodeLocalUploadDoesNotRecreateADeletedServerRoot(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	const uuid = "srv-deleted-mid-backup"
	root := resolveServerRoot(sm, uuid)
	info := storageInfo{Provider: "node-local", Config: json.RawMessage(`{}`)}

	err := uploadBackup(t.Context(), sm, uuid, info, "backups/"+uuid+"/job-1/run-1.tar.gz", strings.NewReader("archive"))

	if err == nil {
		t.Error("upload into a missing server root succeeded")
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("the deleted server's directory is back: %v", err)
	}
}
