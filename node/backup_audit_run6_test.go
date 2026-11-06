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
	pb "dylaris-proto/node"
)

// K20: the live .active_server is carried across the swap, so a whole-server
// restore of a backup that lacks the active sub-server restarted the server
// into a directory the swap deleted. It is refused before the disk changes.
func TestWholeServerRestoreRefusesABackupWithoutTheActiveSubServer(t *testing.T) {
	tests := []struct {
		name   string
		active string
		status string
	}{
		{"active sub-server in the backup", "alpha", "success"},
		{"active sub-server not in the backup", "beta", "failed"},
		{"no active sub-server", "", "success"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, cmd := restoreArchive(t, []tar.Header{
				{Name: "alpha/", Mode: 0o755, Typeflag: tar.TypeDir},
				{Name: "alpha/server.jar", Mode: 0o644, Typeflag: tar.TypeReg},
			}, map[string]string{"alpha/server.jar": "jar"})
			srv := filepath.Join(root, cmd.ServerUUID)
			if tc.active != "" {
				if err := os.MkdirAll(filepath.Join(srv, tc.active), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(srv, ".active_server"), []byte(tc.active), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != tc.status {
				t.Fatalf("report = %v, want %s", report, tc.status)
			}
			if tc.status != "failed" {
				return
			}
			if msg, _ := report["error"].(string); !strings.Contains(msg, `"beta"`) || !strings.Contains(msg, "switch to a sub-server") {
				t.Errorf("error = %q, want it to name the sub-server and say what to do", msg)
			}
			for _, p := range []string{"keep.txt", "beta"} {
				if _, err := os.Stat(filepath.Join(srv, p)); err != nil {
					t.Errorf("the refused restore changed the server: %s: %v", p, err)
				}
			}
			if _, err := os.Stat(filepath.Join(srv, "alpha")); !os.IsNotExist(err) {
				t.Error("the refused restore swapped the archive in")
			}
		})
	}
}

// L22: the archive was checked by path and opened by path again. A
// .dylaris-backups replaced by a link (the tenant's container can rename what
// sits in /data) passed the Lstat of the final name and the open followed it.
func TestBackupDownloadRefusesABackupDirReplacedByALink(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := &StreamHandler{storageMgr: sm}
	const uuid = "srv-l22-link"
	dir, err := h.resolveBackupDir(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "run-7.tar.gz"), []byte("OTHER-TENANT"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, dir)

	var sent []*pb.NodeMessage
	h.streamBackupArchive("r1", uuid, &pb.BackupOpenReq{Key: "run-7.tar.gz"}, func(m *pb.NodeMessage) error { sent = append(sent, m); return nil })
	for _, m := range sent {
		if c := m.GetChunk(); c != nil && strings.Contains(string(c.Data), "OTHER-TENANT") {
			t.Fatal("a file outside the server was streamed out through the backup download")
		}
	}
	if len(sent) != 1 || sent[0].GetError() == nil {
		t.Fatalf("want a single error response, got %d messages", len(sent))
	}

	if f, err := openArchive(filepath.Join(dir, "run-7.tar.gz")); err == nil {
		f.Close()
		t.Fatal("the restore side opened an archive outside the server")
	}
}

// L23: a .partial is no backup. Counted in the usage it held Core's quota, and
// the boot sweep kept a fresh one for six hours although no run survives a
// restart - unless a backup of that server is running right now.
func TestDeadPartialArchivesNeitherCountNorSurviveTheBootSweep(t *testing.T) {
	base := t.TempDir()
	h := &StreamHandler{storageMgr: NewStorageManager(base, nil)}
	const idle, busy = "srv-l23-idle", "srv-l23-busy"
	for _, uuid := range []string{idle, busy} {
		dir, _ := h.resolveBackupDir(uuid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "a.tar.gz"), []byte("12345"), 0o644)
		os.WriteFile(filepath.Join(dir, "b.tar.gz"+partialBackupSuffix), []byte("1234567890"), 0o644)
	}
	if u := h.handleBackupUsage("r1", idle).GetBackupUsageResp(); u.GetUsedBytes() != 5 || u.GetCount() != 1 {
		t.Fatalf("usage = %d bytes in %d, want 5 in 1", u.GetUsedBytes(), u.GetCount())
	}

	if !backupsInFlight.enter("server:" + busy) {
		t.Fatal("busy server already marked")
	}
	defer backupsInFlight.leave("server:" + busy)
	sweepInterruptedWork([]string{base})
	partial := func(uuid string) string {
		return filepath.Join(base, uuid, backupDirName, "b.tar.gz"+partialBackupSuffix)
	}
	if _, err := os.Stat(partial(idle)); !os.IsNotExist(err) {
		t.Error("a fresh .partial of a server with no backup running survived the boot sweep")
	}
	if _, err := os.Stat(partial(busy)); err != nil {
		t.Error("the sweep removed the .partial of a backup that is running")
	}
}

// L24: the spool clear removed every run-* file, also on storage another node
// uses, where one of them is that node's archive mid-upload.
func TestClearBackupSpoolLeavesStorageAnotherNodeUses(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, ".dylaris-backup-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, spoolFileName(812))
	os.WriteFile(live, []byte("x"), 0o600)
	if err := writeBeacon(base, nodeBeacon{NodeID: "other-node", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	clearBackupSpool(dir)
	if _, err := os.Stat(live); err != nil {
		t.Fatal("removed a spool file on storage another node uses")
	}
}
