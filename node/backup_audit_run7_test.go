package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// A player's chat line carries the phrase too, and ended the save wait while
// the flush was still writing region files (M15).
func TestSaveDetectionIgnoresPlayerChat(t *testing.T) {
	for _, tc := range []struct {
		line                 string
		confirmed, unsupport bool
	}{
		{"[12:00:00] [Server thread/INFO]: Saved the game", true, false},
		{"[12:00:00] [Server thread/INFO] [minecraft/DedicatedServer]: Saved the world", true, false},
		{"[12:00:00] [Server thread/INFO]: ThreadedAnvilChunkStorage: All dimensions are saved", true, false},
		{"Saved the game", true, false},
		{"[12:00:00] [Server thread/INFO]: <Steve> Saved the game?", false, false},
		{"[12:00:00] [Server thread/INFO]: <Steve> I Saved the world", false, false},
		{"[12:00:00] [Server thread/INFO]: [Steve] Saved the game", false, false},
		{"[12:00:00] [Server thread/INFO]: <Steve> unknown command lol", false, false},
		{"[12:00:00] [Server thread/INFO]: <Steve> ]: Unknown command", false, false},
		{"[12:00:00] [Server thread/INFO]: Unknown or incomplete command, see below for error", false, true},
		{"[12:00:00 INFO]: Unknown command. Type \"/help\" for help.", false, true},
	} {
		if got := saveConfirmed(tc.line); got != tc.confirmed {
			t.Errorf("saveConfirmed(%q) = %v, want %v", tc.line, got, tc.confirmed)
		}
		if got := saveUnsupported(tc.line); got != tc.unsupport {
			t.Errorf("saveUnsupported(%q) = %v, want %v", tc.line, got, tc.unsupport)
		}
	}
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	gw := gzip.NewWriter(&b)
	_, _ = gw.Write([]byte(s))
	_ = gw.Close()
	return b.Bytes()
}

// A run redelivered after it already succeeded (node killed before the ACK)
// truncated or removed the archive its success row points at (M16). A complete
// archive under the run's key is that run's, so it is reported, not rewritten;
// a truncated one is written again.
func TestARedeliveredBackupKeepsItsCompleteArchive(t *testing.T) {
	const uuid, key = "srv-r7-0001", "backups/srv-r7-0001/job-1/20261106-120000-abcd1234.tar.gz"
	complete := gzipBytes(t, "the archive the success row points at")
	for _, tc := range []struct {
		name, provider string
		existing       []byte
		wantKept       bool
	}{
		{"local, complete", "local", complete, true},
		{"node-local, complete", "node-local", complete, true},
		{"local, truncated", "local", complete[:len(complete)-6], false},
		{"node-local, truncated", "node-local", complete[:len(complete)-6], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, base := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, uuid, "world"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, uuid, "world", "level.dat"), []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := json.RawMessage(`{}`)
			full := filepath.Join(root, uuid, backupDirName, nodeLocalArchiveName(key))
			if tc.provider == "local" {
				cfg, _ = json.Marshal(localCfg{BasePath: base})
				full = filepath.Join(base, filepath.FromSlash(key))
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, tc.existing, 0o644); err != nil {
				t.Fatal(err)
			}
			blob, _ := json.Marshal(storageInfo{ID: 1, Provider: tc.provider, Config: cfg})
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupResultsChannel(nodeID), func() {
				RunBackup(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, BackupRunCommand{
					RunID: 71, JobID: 1, ServerUUID: uuid, StorageKey: key, Storage: blob,
				})
			})
			if report["status"] != "success" {
				t.Fatalf("report = %v, want success", report)
			}
			got, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			if kept := bytes.Equal(got, tc.existing); kept != tc.wantKept {
				t.Fatalf("archive kept = %v, want %v", kept, tc.wantKept)
			}
			if tc.wantKept && report["sizeBytes"] != float64(len(complete)) {
				t.Errorf("reported size %v, want %d", report["sizeBytes"], len(complete))
			}
		})
	}
}

// The old copy is charged to the server's project as long as it exists: a
// recount or a limit put back beside it found the server over its limit right
// after a restore (M22), and a sub-server extracted inside the project under
// its hard limit failed beside the old sub-server (M19).
func TestARestorePutsTheLimitBackOnlyOnceTheOldCopyIsGone(t *testing.T) {
	for _, sub := range []string{"", "A"} {
		t.Run("sub="+sub, func(t *testing.T) {
			var lifted, retagged []string
			var stashAtRetag []string
			prevLift, prevRetag := liftDiskLimitForRestore, retagAfterRestore
			t.Cleanup(func() { liftDiskLimitForRestore, retagAfterRestore = prevLift, prevRetag })

			root, cmd := restoreArchive(t, []tar.Header{
				{Name: "world/level.dat", Mode: 0o644, Typeflag: tar.TypeReg},
			}, map[string]string{"world/level.dat": "w"})
			cmd.SubServer = sub
			if sub != "" {
				if err := os.MkdirAll(filepath.Join(root, cmd.ServerUUID, sub, "world"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			liftDiskLimitForRestore = func(_ context.Context, _ *redis.Client, uuid string) bool {
				lifted = append(lifted, uuid)
				return true
			}
			retagAfterRestore = func(_ context.Context, _ *redis.Client, uuid string) {
				retagged = append(retagged, uuid)
				filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
					if err == nil && strings.Contains(d.Name(), ".pre-restore-") {
						stashAtRetag = append(stashAtRetag, p)
					}
					return nil
				})
			}
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != "success" {
				t.Fatalf("report = %v", report)
			}
			if len(retagged) != 1 {
				t.Fatalf("retagged %v, want once", retagged)
			}
			if len(stashAtRetag) != 0 {
				t.Errorf("the old copy %v was still on disk when the limit went back on", stashAtRetag)
			}
			if wantLift := sub != ""; (len(lifted) == 1) != wantLift {
				t.Errorf("lifted %v, want lifted = %v", lifted, wantLift)
			}
		})
	}
}

// The lift a sub-server restore takes carries the recount's crash marker, so a
// node killed mid-extraction puts the limit back on its next boot, and the
// recount after the restore puts it back and clears the marker.
func TestLiftDiskLimitIsUndoneByTheRecount(t *testing.T) {
	ctx := context.Background()
	rdb := newMiniRedis(t)
	q := &fakeQuota{limitMB: 100, before: usageOf(10*mib, 100*mib), after: usageOf(10*mib, 100*mib)}
	recordDiskLimit(ctx, rdb, "srv", 100)
	if !liftDiskLimit(ctx, rdb, q, "srv") {
		t.Fatal("a limited server was not lifted")
	}
	if q.limitMB != 0 {
		t.Fatalf("hard limit %d MB during the extraction, want lifted", q.limitMB)
	}
	if v, _ := rdb.Get(ctx, diskLiftedKey("srv")).Result(); v != diskLiftedNoGrace {
		t.Fatalf("lifted marker = %q, want %q", v, diskLiftedNoGrace)
	}
	endDiskLift("srv")
	if err := countDiskUsage(ctx, rdb, q, "srv", knownDiskLimitMB(ctx, rdb, q, "srv"), false); err != nil {
		t.Fatal(err)
	}
	if q.limitMB != 100 || rdb.Exists(ctx, diskLiftedKey("srv")).Val() != 0 {
		t.Fatalf("after the recount: limit %d MB, marker left = %v", q.limitMB, rdb.Exists(ctx, diskLiftedKey("srv")).Val() != 0)
	}

	unlimited := &fakeQuota{}
	if liftDiskLimit(ctx, newMiniRedis(t), unlimited, "free") || len(unlimited.setLimits) != 0 {
		t.Fatal("a server with no limit was lifted")
	}
}

// N17: a recount another command ran while a restore held the lift (an
// update_resources beside it) put the hard limit back mid-extraction and
// dropped the marker, so the extraction failed with EDQUOT. It now only
// records the new limit, and the restore's own recount applies it.
func TestARecountDuringARestoreLiftLeavesTheLimitOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		newMB   int64
		wantEnd int64
	}{
		{"raised during the restore", 200, 200},
		{"unchanged", 100, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rdb := newMiniRedis(t)
			q := &fakeQuota{limitMB: 100, before: usageOf(10*mib, 100*mib), after: usageOf(10*mib, 100*mib)}
			recordDiskLimit(ctx, rdb, "srv", 100)
			if !liftDiskLimit(ctx, rdb, q, "srv") {
				t.Fatal("a limited server was not lifted")
			}
			t.Cleanup(func() { endDiskLift("srv") })
			recordDiskLimit(ctx, rdb, "srv", tc.newMB)
			if err := countDiskUsage(ctx, rdb, q, "srv", tc.newMB, false); err != nil {
				t.Fatal(err)
			}
			if q.limitMB != 0 || rdb.Exists(ctx, diskLiftedKey("srv")).Val() != 1 {
				t.Fatalf("during the restore: limit %d MB, marker kept = %v; want lifted and kept", q.limitMB, rdb.Exists(ctx, diskLiftedKey("srv")).Val() == 1)
			}
			endDiskLift("srv")
			if err := countDiskUsage(ctx, rdb, q, "srv", knownDiskLimitMB(ctx, rdb, q, "srv"), false); err != nil {
				t.Fatal(err)
			}
			if q.limitMB != tc.wantEnd || rdb.Exists(ctx, diskLiftedKey("srv")).Val() != 0 {
				t.Fatalf("after the restore: limit %d MB, want %d, marker left = %v", q.limitMB, tc.wantEnd, rdb.Exists(ctx, diskLiftedKey("srv")).Val() != 0)
			}
		})
	}
}
