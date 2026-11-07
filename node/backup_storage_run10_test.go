package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// Q09: a sub-server stage sits in the tenant's /data while the node asks Core
// for the URL and stops the server. A root tenant swapped the created stage
// for a link in that window, and the stage was then reopened by path, so the
// whole archive was extracted wherever the link pointed.
func TestARestoreNeverExtractsThroughAStageSwappedForALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an open directory cannot be removed on Windows, and symlinks need privileges")
	}
	prevLift, prevRetag := liftDiskLimitForRestore, retagAfterRestore
	t.Cleanup(func() { liftDiskLimitForRestore, retagAfterRestore = prevLift, prevRetag })
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/level.dat", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"world/level.dat": "w"})
	cmd.SubServer = "A"
	serverRoot := filepath.Join(root, cmd.ServerUUID)
	if err := os.MkdirAll(filepath.Join(serverRoot, "A", "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	// Runs after the stage is created and before the extraction starts, the
	// window the server stop opens.
	liftDiskLimitForRestore = func(context.Context, *redis.Client, string) bool {
		entries, _ := os.ReadDir(serverRoot)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "A"+stageDirInfix) {
				stage := filepath.Join(serverRoot, e.Name())
				if err := os.Remove(stage); err != nil {
					t.Errorf("remove the stage: %v", err)
				}
				mustSymlink(t, outside, stage)
			}
		}
		return false
	}
	retagAfterRestore = func(context.Context, *redis.Client, string) {}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "failed" {
		t.Errorf("report = %v, want failed", report)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the restore wrote %d entries through the link", len(entries))
	}
	if st, err := os.Lstat(filepath.Join(serverRoot, "A")); err != nil || !st.IsDir() {
		t.Errorf("the sub-server is no longer a real directory: %v, %v", st, err)
	}
}

// Q10: a restore or node-local backup that failed deleted what it wrote and
// still booked it as landed, so every write running beside it went on
// treating that space as used.
func TestAFailedWriteBooksNothingAsLanded(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"restore failing its checksum", func(t *testing.T) {
			root, cmd := restoreArchive(t, []tar.Header{
				{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
			}, map[string]string{"world/r.0.0.mca": "region bytes"})
			var si storageInfo
			var lc localCfg
			_ = json.Unmarshal(cmd.Storage, &si)
			_ = json.Unmarshal(si.Config, &lc)
			full := filepath.Join(lc.BasePath, filepath.FromSlash(cmd.StorageKey))
			b, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)-8] ^= 0xff
			if err := os.WriteFile(full, b, 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != "failed" {
				t.Fatalf("report = %v", report)
			}
		}},
		{"node-local backup cut short", func(t *testing.T) {
			sm := NewStorageManager(t.TempDir(), nil)
			const uuid = "srv-run10-backup"
			if err := os.MkdirAll(resolveServerRoot(sm, uuid), 0o755); err != nil {
				t.Fatal(err)
			}
			info := storageInfo{Provider: "node-local", Config: json.RawMessage(`{}`)}
			r := &failingReader{left: 4096}
			if err := uploadBackup(t.Context(), sm, uuid, info, "backups/"+uuid+"/job-1/run-1.tar.gz", r); err == nil {
				t.Fatal("a broken source reported success")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l0, g0 := landedWrites.Load(), inflightWrites.Load()
			tc.run(t)
			if got := landedWrites.Load() - l0; got != 0 {
				t.Errorf("%d bytes booked as landed, want 0", got)
			}
			if got := inflightWrites.Load() - g0; got != 0 {
				t.Errorf("%d bytes still held", got)
			}
		})
	}
}

// Q12: the held-lift check ran only when a recount started, so a recount
// already walking when a sub-server restore lifted the limit put it back at
// its end, in the middle of the extraction.
func TestARecountRunningWhenARestoreLiftsLeavesTheLimitOff(t *testing.T) {
	ctx := context.Background()
	rdb := newDiskGuardRedis(t)
	recordDiskLimit(ctx, rdb, "srv", 100)
	q := &fakeQuota{limitMB: 100, before: usageOf(10*mib, 100*mib), after: usageOf(10*mib, 100*mib)}
	lifted := false
	q.onTag = func() { lifted = liftDiskLimit(ctx, rdb, q, "srv") }
	t.Cleanup(func() {
		if lifted {
			endDiskLift("srv")
		}
	})
	if err := countDiskUsage(ctx, rdb, q, "srv", 100, false); err != nil {
		t.Fatal(err)
	}
	if !lifted {
		t.Fatal("the restore's lift was refused")
	}
	if q.limitMB != 0 {
		t.Errorf("hard limit %d MB while the restore holds its lift, want 0", q.limitMB)
	}
	if rdb.Exists(ctx, diskLiftedKey("srv")).Val() == 0 {
		t.Error("the recount dropped the lifted marker the restore still needs")
	}
}

// Q13: a server fenced for a storage move after the sweep listed it held the
// sweep on the fence for the whole move, and every other stopped server's
// gauge expired meanwhile.
func TestADiskMeasurementDoesNotWaitOnAStorageMove(t *testing.T) {
	waitRemeasuresIdle(t)
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	sm := NewStorageManager(t.TempDir(), nil)
	globalStorageMgr = sm
	const uuid = "c3d4e5f6-0000-4000-8000-000000000013"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := sm.fenceMove(uuid)
	if err != nil {
		t.Fatal(err)
	}
	rdb := newMiniRedis(t)
	done := make(chan *DiskUsagePayload, 1)
	go func() { done <- getDiskUsage(context.Background(), rdb, uuid, nil) }()
	select {
	case u := <-done:
		release()
		if u != nil {
			t.Errorf("measured %+v mid-move, want no measurement", u)
		}
	case <-time.After(2 * time.Second):
		release()
		<-done
		t.Fatal("the measurement waited on the move")
	}
}
