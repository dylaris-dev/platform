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

	"github.com/redis/go-redis/v9"
)

// R29-04: the spool's free space was read before the source walk and outside
// spoolReserveMu, so a spool or restore that released (moving its bytes from
// inflight to landed) during the walk vanished from both the stale reading and
// the inflight count. The reading now happens under the lock.
func TestReserveSpoolReadsFreeSpaceUnderTheLock(t *testing.T) {
	const gb = 1_000_000_000
	on := &backupPolicy{SpoolEnabled: true, SpoolMaxGB: 100}
	t.Cleanup(func() { writesOn("").inflight.Store(0) })
	writesOn("").inflight.Store(0)
	locked := false
	_, release, _, why := reserveSpool(on, 1*gb, func() uint64 {
		if spoolReserveMu.TryLock() {
			spoolReserveMu.Unlock()
		} else {
			locked = true
		}
		return 500 * gb
	})
	defer release(0)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	if !locked {
		t.Error("free space was read without holding spoolReserveMu")
	}
}

// R29-05: a failure after the stop was reported before the deferred restart
// and recount ran; a node killed in between had the restore redelivered and
// re-run over a row the panel already showed as failed.
func TestRestoreReportsFailureOnlyAfterItsDeferredWork(t *testing.T) {
	prevLift, prevRetag := liftDiskLimitForRestore, retagAfterRestore
	t.Cleanup(func() { liftDiskLimitForRestore, retagAfterRestore = prevLift, prevRetag })
	// Only a directory: fails with "archive contained no regular files",
	// after the lift and its deferred recount are in place.
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/", Mode: 0o755, Typeflag: tar.TypeDir},
	}, nil)
	cmd.SubServer = "A"
	if err := os.MkdirAll(filepath.Join(root, cmd.ServerUUID, "A", "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb := newMiniRedis(t)
	early := rdb.Subscribe(context.Background(), queue.BackupRestoresChannel(nodeID))
	t.Cleanup(func() { early.Close() })
	if _, err := early.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	liftDiskLimitForRestore = func(context.Context, *redis.Client, string) bool { return true }
	var reportedBeforeRetag bool
	retagAfterRestore = func(context.Context, *redis.Client, string) {
		for {
			msg, err := early.ReceiveTimeout(context.Background(), 300*time.Millisecond)
			if err != nil {
				return
			}
			if m, ok := msg.(*redis.Message); ok && strings.Contains(m.Payload, `"failed"`) {
				reportedBeforeRetag = true
				return
			}
		}
	}
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "failed" || !strings.Contains(report["error"].(string), "no regular files") {
		t.Fatalf("report = %v", report)
	}
	if reportedBeforeRetag {
		t.Error("failure was reported before the restore's deferred work finished")
	}
}

func freeIs(n uint64) func() uint64 { return func() uint64 { return n } }
