package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// R36-04: a graceful stop or restart during the walk sends save-all and then
// "stop", which saves every world while tar reads it. Saving cannot be held
// off there, so the run must not report the torn archive as a success.
func TestAStopDuringTheWalkFailsTheMultipartRun(t *testing.T) {
	prevPause, prevPolicy := pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() { pauseSaves = prevPause; backupPolicyNow.Store(prevPolicy) })
	backupPolicyNow.Store(nil) // no spool: the walk runs for the whole upload
	const uuid = "srv-f3-0001" // the run runBackupWithFakeCore starts
	pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, id, _ string) *worldSaveGuard {
		return &worldSaveGuard{uuid: id, send: func(string) error { return nil }}
	}
	for _, tc := range []struct {
		name     string
		stopSub  string
		stop     bool
		wantFail bool
	}{
		{"no stop", "", false, false},
		{"the archived server is stopped", "", true, true},
		{"a sub-server of it is stopped", "world", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := newFakeCore(t, 1024)
			core.bucket.fail = func(part int32, _ string) int {
				if part == 1 && tc.stop {
					stoppingDuringBackup(uuid, tc.stopSub)
				}
				return 0
			}
			report := runBackupWithFakeCore(t, core, nil)
			if failed := report["status"] == "failed"; failed != tc.wantFail {
				t.Fatalf("report = %v, want failed = %v", report, tc.wantFail)
			}
			if tc.wantFail {
				if msg, _ := report["error"].(string); !strings.Contains(msg, "stopped or restarted") {
					t.Errorf("error = %q, want it to say the server was stopped", msg)
				}
				if n := core.completeCount(); n != 0 {
					t.Errorf("the torn upload was completed %d times", n)
				}
			}
			// After the run a stop is an ordinary stop again.
			stoppingDuringBackup(uuid, tc.stopSub)
		})
	}
}

// The guard on its own: a stop of another sub-server leaves the run alone, a
// stop once the archive no longer reads the tree (a complete spool) too.
func TestStopDuringWalkGuard(t *testing.T) {
	g := &worldSaveGuard{uuid: "srv-stop"}
	stop := g.pauseOnStart(func(string) error { return nil }, "survival", newBackupPacer(0))
	stoppingDuringBackup("srv-stop", "creative")
	if g.stoppedDuringWalk() {
		t.Fatal("a stop of another sub-server tainted the run")
	}
	stoppingDuringBackup("srv-stop", "survival")
	if !g.stoppedDuringWalk() {
		t.Fatal("a stop of the archived sub-server did not taint the run")
	}
	stop()

	g2 := &worldSaveGuard{uuid: "srv-stop"}
	stop2 := g2.pauseOnStart(func(string) error { return nil }, "", newBackupPacer(0))
	stop2()
	stoppingDuringBackup("srv-stop", "")
	if g2.stoppedDuringWalk() {
		t.Fatal("a stop after the walk tainted the run")
	}
}

// A filesystem target holds a real file under the run's key: a stopped run
// deletes it rather than leaving a torn archive that looks complete.
func TestAStopDuringTheWalkFailsTheFilesystemRun(t *testing.T) {
	prevPause := pauseSaves
	t.Cleanup(func() { pauseSaves = prevPause })
	const uuid, key = "srv-r36-0004", "backups/srv-r36-0004/job-1/x.tar.gz"
	for _, tc := range []struct {
		name    string
		stopped bool
	}{
		{"not stopped", false},
		{"stopped during the walk", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, id, _ string) *worldSaveGuard {
				// Stands in for a stop that reached the guard mid-walk.
				return &worldSaveGuard{uuid: id, stopped: tc.stopped}
			}
			root, base := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, uuid, "world"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, uuid, "world", "level.dat"), []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, _ := json.Marshal(localCfg{BasePath: base})
			blob, _ := json.Marshal(storageInfo{ID: 1, Provider: "local", Config: cfg})
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupResultsChannel(nodeID), func() {
				RunBackup(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, BackupRunCommand{
					RunID: 364, JobID: 1, ServerUUID: uuid, StorageKey: key, Storage: blob,
				})
			})
			_, statErr := os.Stat(filepath.Join(base, filepath.FromSlash(key)))
			if tc.stopped {
				if report["status"] != "failed" || statErr == nil {
					t.Fatalf("report = %v, archive left = %v; want failed and no archive", report, statErr == nil)
				}
				return
			}
			if report["status"] != "success" || statErr != nil {
				t.Fatalf("report = %v, archive stat = %v; want success with the archive", report, statErr)
			}
		})
	}
}

// R39-02: an unspooled run whose archive is complete no longer reads the tree,
// so a stop while its last part uploads is an ordinary stop: the run succeeds,
// and saving was already back on. One part larger than the archive is only
// sent once the walk has ended.
func TestAStopAfterTheUnspooledArchiveIsCompleteKeepsTheRun(t *testing.T) {
	prevPause, prevPolicy := pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() { pauseSaves = prevPause; backupPolicyNow.Store(prevPolicy) })
	backupPolicyNow.Store(nil)
	const uuid = "srv-f3-0001"
	var mu sync.Mutex
	var sent []string
	pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, id, _ string) *worldSaveGuard {
		return &worldSaveGuard{uuid: id, send: func(c string) error {
			mu.Lock()
			sent = append(sent, c)
			mu.Unlock()
			return nil
		}}
	}
	core := newFakeCore(t, 1<<20)
	resumed := false
	core.bucket.fail = func(int32, string) int {
		mu.Lock()
		resumed = slices.Contains(sent, "save-on")
		mu.Unlock()
		stoppingDuringBackup(uuid, "")
		return 0
	}
	report := runBackupWithFakeCore(t, core, nil)
	if report["status"] != "success" {
		t.Fatalf("report = %v, want success", report)
	}
	if !resumed {
		t.Error("saving was still off while the last part uploaded")
	}
}
