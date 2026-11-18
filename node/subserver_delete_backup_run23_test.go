package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"
)

// R23-03: a sub-server delete that met a running backup was only logged, while
// Core had already dropped its rows: the slot stayed on disk for good. It now
// waits for the backup, bounded, and gives up only at the bound.
func TestASubServerDeleteWaitsForTheBackup(t *testing.T) {
	prevPoll, prevMax := treeChangePoll, treeChangeWaitMax
	t.Cleanup(func() { treeChangePoll, treeChangeWaitMax = prevPoll, prevMax })
	treeChangePoll = 5 * time.Millisecond

	for _, tc := range []struct {
		name       string
		backupFor  time.Duration // how long the backup still runs
		waitMax    time.Duration
		wantLeaves bool
	}{
		{"idle server", 0, time.Second, true},
		{"backup ends within the bound", 50 * time.Millisecond, 5 * time.Second, true},
		{"backup outlasts the bound", time.Hour, 30 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uuid := "r23-03-" + strings.ReplaceAll(tc.name, " ", "-")
			treeChangeWaitMax = tc.waitMax
			if tc.backupFor > 0 {
				if reason := enterBackupOfServer(uuid); reason != "" {
					t.Fatal(reason)
				}
				timer := time.AfterFunc(tc.backupFor, func() { backupsInFlight.leave("server:" + uuid) })
				t.Cleanup(func() {
					if timer.Stop() {
						backupsInFlight.leave("server:" + uuid)
					}
				})
			}
			leave, reason := waitTreeChangeOfServer(context.Background(), uuid)
			if got := reason == ""; got != tc.wantLeaves {
				t.Fatalf("entered = %v (reason %q), want %v", got, reason, tc.wantLeaves)
			}
			if leave != nil {
				leave()
			}
		})
	}
}

// The delete case waits for the interlock instead of refusing on the first try.
func TestDeleteSubServerWaitsForTheTreeInterlock(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(s, "\tcase \"delete_sub_server\":")
	j := strings.Index(s[i:], "\tcase \"reinstall\":")
	if i < 0 || j < 0 {
		t.Fatal("the delete_sub_server case moved; move this assertion with it")
	}
	if !strings.Contains(s[i:i+j], "waitTreeChangeOfServer(ctx, cmd.Config.UUID)") {
		t.Error("delete_sub_server refuses on a running backup instead of waiting for it")
	}
}

// R23-04: a backup of a server that is moving off this node wrote an archive
// into a tree the move's cleanup deletes, or broke the staging by counting as a
// write. It is refused, and leaves the move's write watch untouched.
func TestABackupIsRefusedWhileTheServerMoves(t *testing.T) {
	for _, tc := range []struct {
		name        string
		staging     bool // a migrate_out of the server is running
		staged      bool // its archive is staged, cleanup still to come
		wantRefused bool
	}{
		{"no move", false, false, false},
		{"move staging", true, false, true},
		{"move staged", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uuid := "r23-04-" + strings.ReplaceAll(tc.name, " ", "-")
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, uuid, "world"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, uuid, "world", "level.dat"), []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			sm := &StorageManager{paths: []string{root}}
			if tc.staging {
				run := "attempt"
				migrateOutLatest.Store(uuid, &run)
				t.Cleanup(func() { migrateOutLatest.Delete(uuid) })
			}
			if tc.staged {
				if err := os.MkdirAll(filepath.Join(root, migrationStagingDir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stagedArchivePath(root, uuid), []byte("zip"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writesSince, err := sm.watchWrites(uuid)
			if err != nil {
				t.Fatal(err)
			}
			const key = "backups/x/job-1/20261118-120000-abcd1234.tar.gz"
			blob, _ := json.Marshal(storageInfo{ID: 1, Provider: "node-local", Config: json.RawMessage(`{}`)})
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupResultsChannel(nodeID), func() {
				RunBackup(context.Background(), rdb, sm, nil, BackupRunCommand{
					RunID: 2304, JobID: 1, ServerUUID: uuid, StorageKey: key, Storage: blob,
				})
			})
			refused := report["status"] == "failed" && strings.Contains(report["error"].(string), "being moved")
			if refused != tc.wantRefused {
				t.Fatalf("report = %v, want refused = %v", report, tc.wantRefused)
			}
			if tc.wantRefused {
				if writesSince() {
					t.Error("a refused backup still counted as a write and fails the move's staging")
				}
				if _, err := os.Stat(filepath.Join(root, uuid, backupDirName)); err == nil {
					t.Error("a refused backup wrote into the tree the move deletes")
				}
			}
		})
	}
}
