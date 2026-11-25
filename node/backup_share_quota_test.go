package main

import (
	"context"
	"encoding/json"
	"fmt"
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
		backupShareQuotaMem.Store(0)
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

	on, off := true, false
	for _, tc := range []struct {
		name       string
		share      []*bool // the runs Core sent, in order; nil is an older Core
		restart    bool    // the in-memory mode is lost, the persisted one is not
		wantBackup bool
	}{
		// An older Core never sends the field and still skips its own per-server
		// cap in share mode, so the archives stay charged to the server, as
		// before the field existed.
		{"never told (older Core)", []*bool{nil}, false, true},
		{"never told, after a node restart", []*bool{nil}, true, true},
		{"separate budget", []*bool{&off}, false, false},
		{"separate budget, after a node restart", []*bool{&off}, true, false},
		{"shared with the server", []*bool{&on}, false, true},
		{"shared, after a node restart", []*bool{&on}, true, true},
		{"separate again after sharing", []*bool{&on, &off}, true, false},
		{"an older Core leaves an explicit separate budget alone", []*bool{&off, nil}, true, false},
		{"an older Core leaves an explicit share alone", []*bool{&on, nil}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeSecretDir = t.TempDir()
			backupShareQuotaMem.Store(0)
			for _, s := range tc.share {
				setBackupShareQuota(s)
			}
			if tc.restart {
				backupShareQuotaMem.Store(0)
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
		want string
	}{
		{`{"runId":1}`, "absent"}, // an older Core: must stay distinguishable from false
		{`{"runId":1,"shareQuotaWithServer":false}`, "false"},
		{`{"runId":1,"shareQuotaWithServer":true}`, "true"},
	} {
		var cmd BackupRunCommand
		if err := json.Unmarshal([]byte(tc.raw), &cmd); err != nil {
			t.Fatal(err)
		}
		got := "absent"
		if cmd.ShareQuotaWithServer != nil {
			got = fmt.Sprintf("%t", *cmd.ShareQuotaWithServer)
		}
		if got != tc.want {
			t.Errorf("%s: ShareQuotaWithServer = %s, want %s", tc.raw, got, tc.want)
		}
	}
}
