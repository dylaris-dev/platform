package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// B06: node-local archives live inside the server directory, and the du path
// counted them toward the server's own disk limit, so the disk guard stopped a
// server whose backups Core treats as a separate budget. They count only when
// Core says the backup quota is shared with the server, and that answer must
// survive a node restart.
func TestDuDiskUsageCountsBackupsOnlyWhenShared(t *testing.T) {
	prevNodeID, prevMgr, prevSecret := nodeID, globalStorageMgr, nodeSecretDir
	t.Cleanup(func() {
		nodeID, globalStorageMgr, nodeSecretDir = prevNodeID, prevMgr, prevSecret
		backupShareQuotaMem.Store(false)
	})
	nodeID = "node-1"

	ctx := context.Background()
	rdb := newDiskGuardRedis(t)
	const uuid = "srv-b06-0001"
	const archive = 1 << 20

	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(filepath.Join(dir, backupDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "world.dat"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupDirName, "run-1.tar.gz"), make([]byte, archive), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		share      bool
		restart    bool // the in-memory mode is lost, the persisted one is not
		wantBackup bool
	}{
		{"separate budget (default, older Core)", false, false, false},
		{"shared with the server", true, false, true},
		{"shared, after a node restart", true, true, true},
		{"separate again after sharing", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeSecretDir = t.TempDir()
			setBackupShareQuota(tc.share)
			if tc.restart {
				backupShareQuotaMem.Store(false)
			}
			usage := duDiskUsage(ctx, rdb, uuid)
			if usage == nil {
				t.Fatal("duDiskUsage returned nil")
			}
			if got := usage.Total >= archive; got != tc.wantBackup {
				t.Fatalf("Total = %d, archives counted = %v, want %v", usage.Total, got, tc.wantBackup)
			}
		})
	}
}

func TestBackupRunCommandCarriesShareQuota(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"runId":1}`, false}, // an older Core sends nothing: separate budget
		{`{"runId":1,"shareQuotaWithServer":true}`, true},
	} {
		var cmd BackupRunCommand
		if err := json.Unmarshal([]byte(tc.raw), &cmd); err != nil {
			t.Fatal(err)
		}
		if cmd.ShareQuotaWithServer != tc.want {
			t.Errorf("%s: ShareQuotaWithServer = %v, want %v", tc.raw, cmd.ShareQuotaWithServer, tc.want)
		}
	}
}
