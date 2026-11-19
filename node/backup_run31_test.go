package main

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"
)

// R31-04: a whole-server restore of a server whose root does not resolve (two
// copies, no storage key) staged in the node's working directory, stopped the
// server and unpacked the archive there before failing at the swap. It is now
// refused before any of that.
func TestARestoreRefusesAnUnresolvedServerRoot(t *testing.T) {
	_, cmd := restoreArchive(t, []tar.Header{
		{Name: "a.txt", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"a.txt": "x"})
	cmd.ServerUUID = "3f2b7c1e-0d4a-4b8e-9c6f-1a2b3c4d5e6f"
	a, b := t.TempDir(), t.TempDir()
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(p, cmd.ServerUUID), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sm := &StorageManager{paths: []string{a, b}}
	if dir := sm.GetServerDir(cmd.ServerUUID); dir != "" {
		t.Fatalf("precondition: root resolved to %q", dir)
	}
	cwd, _ := os.Getwd()
	before, _ := filepath.Glob(filepath.Join(cwd, "..restore-*"))
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, sm, nil, cmd)
	})
	if report["status"] != "failed" || !strings.Contains(report["error"].(string), "cannot be resolved") {
		t.Fatalf("report = %v", report)
	}
	if after, _ := filepath.Glob(filepath.Join(cwd, "..restore-*")); len(after) != len(before) {
		t.Errorf("a stage was left in the working directory: %v", after)
	}
	if n, _ := rdb.Exists(context.Background(), "dylaris:server:"+cmd.ServerUUID+":status").Result(); n != 0 {
		t.Error("the refused restore touched the server's status")
	}
}

// R31-06: the first policy load ran inside the watcher goroutine main started
// after the command consumer, so a spool-enabled backup redelivered at boot
// read no policy and uploaded straight from the live tree. The watcher now
// returns only once the policy is loaded, and main calls it before the
// consumer.
func TestWatchBackupPolicyReturnsWithThePolicyLoaded(t *testing.T) {
	prev := backupPolicyNow.Load()
	t.Cleanup(func() { backupPolicyNow.Store(prev) })
	backupPolicyNow.Store(nil)
	rdb := newMiniRedis(t)
	if err := rdb.Set(context.Background(), backupPolicyKey(nodeID), `{"spoolEnabled":true,"spoolMaxGB":5}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() { watchBackupPolicy(ctx, rdb, nodeID, spool); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchBackupPolicy did not return after its first load; main cannot run it before the consumer")
	}
	if p := backupPolicyNow.Load(); p == nil || !p.SpoolEnabled {
		t.Fatalf("policy after return = %+v, want the stored spool policy", p)
	}
}
