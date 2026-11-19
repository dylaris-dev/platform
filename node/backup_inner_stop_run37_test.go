package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// R37-05/06: a stop or save started from inside the server (console, in-game
// /stop or /save-all, a restart plugin) writes the world under the tar without
// any node path running. The console the walk already tails says so.
func TestAStopOrSaveFromInsideTheServerTaintsTheWalk(t *testing.T) {
	const uuid = "srv-r37-0005"
	for _, tc := range []struct {
		name string
		// ownSave: the guard queued a save-all of its own (a start mid-walk).
		ownSave bool
		lines   []string
		want    bool
	}{
		{"quiet console", false, []string{"[12:00:00] [Server thread/INFO]: Steve joined the game"}, false},
		{"console stop, vanilla", false, []string{"[12:00:00] [Server thread/INFO]: Stopping server"}, true},
		{"console stop, Paper", false, []string{"[12:00:00 INFO]: Stopping server"}, true},
		{"chat that says it", false, []string{"[12:00:00] [Server thread/INFO]: <Steve> Stopping server"}, false},
		{"console save-all", false, []string{"[12:00:00] [Server thread/INFO]: Saving the game (this may take a moment!)", "[12:00:01] [Server thread/INFO]: Saved the game"}, true},
		{"console save-all, 1.12", false, []string{"[12:00:00] [Server thread/INFO]: Saving...", "[12:00:01] [Server thread/INFO]: Saved the world"}, true},
		{"in-game save-all by an op", false, []string{"[12:00:01] [Server thread/INFO]: [Steve: Saved the game]"}, true},
		{"/say that says it", false, []string{"[12:00:01] [Server thread/INFO]: [Steve] Saved the game"}, false},
		{"the guard's own save-all", true, []string{"[12:00:00] [Server thread/INFO]: Saving the game (this may take a moment!)", "[12:00:01] [Server thread/INFO]: ThreadedAnvilChunkStorage: All dimensions are saved", "[12:00:01] [Server thread/INFO]: Saved the game"}, false},
		{"one more save after the guard's own", true, []string{"[12:00:00] [Server thread/INFO]: Saving the game (this may take a moment!)", "[12:00:05] [Server thread/INFO]: Saving the game (this may take a moment!)"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			key := serverLogStreamKey(uuid, "")
			rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: key, Values: map[string]any{"line": "[11:59:00] [Server thread/INFO]: Stopping server"}})
			g := &worldSaveGuard{uuid: uuid, send: func(string) error { return nil }, subs: map[string]bool{"": true}}
			stopPause := g.pauseOnStart(func(string) error { return nil }, "", newBackupPacer(0))
			defer stopPause()
			if tc.ownSave {
				startingDuringBackup(uuid, "")
			}
			stopWatch := g.watchCrashRestarts(rdb)
			defer stopWatch()
			// The watcher reads from the newest line once it has the stream.
			time.Sleep(400 * time.Millisecond)
			for _, l := range tc.lines {
				rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: key, Values: map[string]any{"line": l}})
			}
			// Long enough for the watcher's 250 ms read to come round twice.
			deadline := time.Now().Add(700 * time.Millisecond)
			for !g.stoppedDuringWalk() && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if got := g.stoppedDuringWalk(); got != tc.want {
				t.Fatalf("stopped = %v, want %v", got, tc.want)
			}
		})
	}
}

// R37-07: a node-local run that fails as stopped during the walk, or as
// matching nothing, books none of its removed archive as landed: those bytes
// would keep counting against every other write on the disk.
func TestAFailedNodeLocalRunBooksNothingAsLanded(t *testing.T) {
	prevPause := pauseSaves
	t.Cleanup(func() { pauseSaves = prevPause })
	const uuid = "srv-r37-0007"
	for _, tc := range []struct {
		name    string
		stopped bool
		include []string
	}{
		{"stopped during the walk", true, nil},
		{"nothing matched", false, []string{"nothing-is-called-this/**"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, id, _ string) *worldSaveGuard {
				return &worldSaveGuard{uuid: id, stopped: tc.stopped}
			}
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, uuid, "world"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, uuid, "world", "level.dat"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, uuid, backupDirName)
			before := writesOn(dir).landed.Load()
			blob, _ := json.Marshal(storageInfo{ID: 1, Provider: "node-local", Config: json.RawMessage(`{}`)})
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupResultsChannel(nodeID), func() {
				RunBackup(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, BackupRunCommand{
					RunID: 370, JobID: 1, ServerUUID: uuid, StorageKey: "backups/" + uuid + "/job-1/x.tar.gz", Storage: blob,
					IncludePatterns: tc.include,
				})
			})
			if report["status"] != "failed" {
				t.Fatalf("report = %v, want failed", report)
			}
			if got := writesOn(dir).landed.Load() - before; got != 0 {
				t.Errorf("%d bytes of the removed archive are still booked as landed", got)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("left in the backup dir: %v", entries)
			}
		})
	}
}
