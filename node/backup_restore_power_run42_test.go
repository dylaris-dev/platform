package main

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// R42-03: a restart already in the command stream ran beside a restore, which
// took no power lock: the JVM came back up on the old tree, and the swap then
// deleted the directory it was writing into. The restore now waits for a power
// action of the server to finish and holds the lock until after its restart.
func TestRestoreHoldsThePowerLockOfItsServer(t *testing.T) {
	prevLift, prevRetag := liftDiskLimitForRestore, retagAfterRestore
	t.Cleanup(func() { liftDiskLimitForRestore, retagAfterRestore = prevLift, prevRetag })
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "a.txt", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"a.txt": "restored"})
	cmd.SubServer = "A"
	sub := filepath.Join(root, cmd.ServerUUID, "A")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "live.txt"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Called after the restore's stop, before the swap: a power action must not
	// be able to take the server here.
	lockFreeMidRestore := false
	liftDiskLimitForRestore = func(context.Context, *redis.Client, string) bool {
		if release, ok := tryLockServerPower(cmd.ServerUUID); ok {
			lockFreeMidRestore = true
			release()
		}
		return true
	}
	retagAfterRestore = func(context.Context, *redis.Client, string) {}

	// A restart of the server is running when the restore comes in.
	releaseRestart, ok := tryLockServerPower(cmd.ServerUUID)
	if !ok {
		t.Fatal("power lock not free at the start")
	}
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
		}()
		select {
		case <-done:
			t.Error("the restore ran while a power action of its server held the lock")
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := os.Stat(filepath.Join(sub, "live.txt")); err != nil {
			t.Errorf("the live tree was swapped while the power action ran: %v", err)
		}
		releaseRestart()
		<-done
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	if lockFreeMidRestore {
		t.Error("a power action could take the server's lock in the middle of the restore")
	}
	if got, err := os.ReadFile(filepath.Join(sub, "a.txt")); err != nil || string(got) != "restored" {
		t.Errorf("a.txt = %q, %v", got, err)
	}
	if release, ok := tryLockServerPower(cmd.ServerUUID); !ok {
		t.Error("the restore did not release the power lock")
	} else {
		release()
	}
}
