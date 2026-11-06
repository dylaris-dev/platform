package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"

	pb "dylaris-proto/node"
)

// The tenant is root on its own /data when MC_RUN_AS=0, so it can replace
// .dylaris-backups itself with a link. Every write, delete and list on that
// directory then went wherever the link pointed: the node wrote an archive into
// a host directory of the tenant's choosing, removed files there by name, and
// reported that directory's files to Core as the server's archives.
func TestNodeLocalBackupRefusesALinkedBackupDir(t *testing.T) {
	for _, op := range []string{"upload", "deleteBackup", "rpcDelete", "list", "usage"} {
		t.Run(op, func(t *testing.T) {
			sm := NewStorageManager(t.TempDir(), nil)
			h := &StreamHandler{storageMgr: sm}
			const uuid = "srv-linked-dir"
			serverDir := resolveServerRoot(sm, uuid)
			if err := os.MkdirAll(serverDir, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "victim.tar.gz"), []byte("0123456789"), 0o644); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, outside, filepath.Join(serverDir, backupDirName))
			info := storageInfo{Provider: "node-local", Config: json.RawMessage(`{}`)}

			switch op {
			case "upload":
				err := uploadBackup(t.Context(), sm, uuid, info, "backups/"+uuid+"/job-1/run-1.tar.gz", strings.NewReader("ARCHIVE"))
				if err == nil {
					t.Error("upload through a linked backup dir succeeded")
				}
			case "deleteBackup":
				deleteBackup(t.Context(), sm, uuid, info, "backups/x/victim.tar.gz")
			case "rpcDelete":
				if m := h.handleBackupDelete("r", uuid, &pb.BackupDeleteReq{Key: "victim.tar.gz"}); m.GetError() == nil {
					t.Error("delete through a linked backup dir reported success")
				}
			case "list":
				if l := h.handleBackupList("r", uuid).GetBackupListResp(); l != nil && len(l.Objects) != 0 {
					t.Errorf("listed %v from the link's target", l.Objects)
				}
			case "usage":
				if u := h.handleBackupUsage("r", uuid).GetBackupUsageResp(); u != nil && u.UsedBytes != 0 {
					t.Errorf("usage %d bytes from the link's target", u.UsedBytes)
				}
			}

			entries, _ := os.ReadDir(outside)
			if len(entries) != 1 || entries[0].Name() != "victim.tar.gz" {
				names := []string{}
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("link target now holds %v, want only victim.tar.gz", names)
			}
		})
	}
}

// A real directory still works for every operation the guard now covers.
func TestNodeLocalBackupRoundTripInARealDir(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := &StreamHandler{storageMgr: sm}
	const uuid = "srv-real-dir"
	if err := os.MkdirAll(resolveServerRoot(sm, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	info := storageInfo{Provider: "node-local", Config: json.RawMessage(`{}`)}
	if err := uploadBackup(t.Context(), sm, uuid, info, "backups/"+uuid+"/job-1/run-1.tar.gz", strings.NewReader("ARCHIVE")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if l := h.handleBackupList("r", uuid).GetBackupListResp(); l == nil || len(l.Objects) != 1 || l.Objects[0].Key != "run-1.tar.gz" {
		t.Fatalf("list = %v", l)
	}
	if u := h.handleBackupUsage("r", uuid).GetBackupUsageResp(); u == nil || u.UsedBytes != int64(len("ARCHIVE")) {
		t.Fatalf("usage = %v", u)
	}
	if m := h.handleBackupDelete("r", uuid, &pb.BackupDeleteReq{Key: "run-1.tar.gz"}); m.GetError() != nil {
		t.Fatalf("delete: %v", m.GetError())
	}
	if m := h.handleBackupDelete("r", uuid, &pb.BackupDeleteReq{Key: "run-1.tar.gz"}); m.GetError().GetCode() != 404 {
		t.Fatalf("second delete = %v, want 404", m.GetError())
	}
	if err := uploadBackup(t.Context(), sm, uuid, info, "backups/"+uuid+"/job-1/run-2.tar.gz", strings.NewReader("B")); err != nil {
		t.Fatal(err)
	}
	deleteBackup(t.Context(), sm, uuid, info, "backups/"+uuid+"/job-1/run-2.tar.gz")
	if l := h.handleBackupList("r", uuid).GetBackupListResp(); l == nil || len(l.Objects) != 0 {
		t.Fatalf("list after deleteBackup = %v", l)
	}
}

// The success report went out before the deferred restart and recount ran, and
// the command is ACKed only when the handler returns: a node killed in between
// (a Watchtower update) got the restore redelivered and applied it again over
// everything played since, or, presigned, reported it failed over Core's
// success row.
func TestRestoreReportsSuccessOnlyAfterItsDeferredWork(t *testing.T) {
	prevLift, prevRetag := liftDiskLimitForRestore, retagAfterRestore
	t.Cleanup(func() { liftDiskLimitForRestore, retagAfterRestore = prevLift, prevRetag })
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "world/level.dat", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string]string{"world/level.dat": "w"})
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
			if m, ok := msg.(*redis.Message); ok && strings.Contains(m.Payload, `"success"`) {
				reportedBeforeRetag = true
				return
			}
		}
	}
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	if reportedBeforeRetag {
		t.Error("success was reported before the restore's deferred work finished")
	}
}
