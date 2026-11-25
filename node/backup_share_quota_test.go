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

// R52-02/R52-06: the backup folder's project follows the share mode for every
// server, not only one that makes a node-local backup: at boot (the recount
// skips counted servers) and when a backup_run changes the mode.
func TestBackupFoldersFollowTheShareMode(t *testing.T) {
	prevNodeID, prevSecret, prevSet, prevTag := nodeID, nodeSecretDir, globalQuotaSet, setBackupDirProject
	t.Cleanup(func() {
		nodeID, nodeSecretDir, globalQuotaSet, setBackupDirProject = prevNodeID, prevSecret, prevSet, prevTag
		backupShareQuotaMem.Store(0)
	})
	nodeID = "node-1"
	nodeSecretDir = t.TempDir()
	backupShareQuotaMem.Store(0)

	ctx := context.Background()
	rdb := newDiskGuardRedis(t)
	const withBackups = "0b6c1a52-0000-4000-8000-000000000001"
	const without = "0b6c1a52-0000-4000-8000-000000000002"
	root := t.TempDir()
	storage := NewStorageManager(root, nil)
	if err := os.MkdirAll(filepath.Join(root, withBackups, backupDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, without), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{withBackups, without} {
		markDiskCounted(ctx, rdb, uuid) // counted: the boot recount skips both
	}

	tags := map[string]uint32{}
	setBackupDirProject = func(dir string, pid uint32, share bool) error {
		if pid != projectID(filepath.Base(filepath.Dir(dir))) {
			t.Errorf("%s tagged with server project %d", dir, pid)
		}
		if !share {
			pid = 0
		}
		tags[dir] = pid
		return nil
	}
	backupDir := filepath.Join(root, withBackups, backupDirName)
	qp := &QuotaProvider{basePath: root, available: true}
	globalQuotaSet = &QuotaSet{storage: storage, providers: map[string]*QuotaProvider{root: qp}}

	off, on := false, true
	setBackupShareQuota(&off) // persisted mode, before the node restart
	clear(tags)
	backupShareQuotaMem.Store(0)

	for _, tc := range []struct {
		name string
		step func()
		want map[string]uint32
	}{
		{"boot: counted server's folder comes off the server", func() { qp.reconcileExisting(rdb, storage) }, map[string]uint32{backupDir: 0}},
		{"mode changes to share", func() { setBackupShareQuota(&on) }, map[string]uint32{backupDir: projectID(withBackups)}},
		{"same mode again: no pass", func() { setBackupShareQuota(&on) }, map[string]uint32{}},
		{"an older Core: no pass", func() { setBackupShareQuota(nil) }, map[string]uint32{}},
		{"mode changes back to separate", func() { setBackupShareQuota(&off) }, map[string]uint32{backupDir: 0}},
	} {
		clear(tags)
		tc.step()
		if fmt.Sprint(tags) != fmt.Sprint(tc.want) {
			t.Errorf("%s: tagged %v, want %v", tc.name, tags, tc.want)
		}
	}
}

// R54-02/R54-04: the share mode rides on the backup policy Core republishes
// to every node, so a node with no backups still learns a change, and once a
// policy has carried it a backup_run (a redelivered old one included) no
// longer overrides it. A policy without the field is an older Core: the run
// still decides.
func TestBackupPolicyCarriesTheShareMode(t *testing.T) {
	prevSecret, prevSeen, prevPolicy := nodeSecretDir, backupSharePolicySeen.Load(), backupPolicyNow.Load()
	t.Cleanup(func() {
		nodeSecretDir = prevSecret
		backupShareQuotaMem.Store(0)
		backupSharePolicySeen.Store(prevSeen)
		backupPolicyNow.Store(prevPolicy)
	})
	on, off := true, false
	for _, tc := range []struct {
		name   string
		policy string  // "" is no policy key
		runs   []*bool // backup_runs after the policy load
		want   bool
	}{
		{"policy turns sharing off without a run", `{"shareQuotaWithServer":false}`, nil, false},
		{"policy turns sharing on without a run", `{"shareQuotaWithServer":true}`, nil, true},
		{"redelivered old run cannot flip the policy's share", `{"shareQuotaWithServer":true}`, []*bool{&off}, true},
		{"redelivered old run cannot flip the policy's separate budget", `{"shareQuotaWithServer":false}`, []*bool{&on}, false},
		{"older Core policy: the run decides", `{"headroomPct":30}`, []*bool{&off}, false},
		{"no policy: the run decides", "", []*bool{&off}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeSecretDir = t.TempDir()
			backupShareQuotaMem.Store(0)
			backupSharePolicySeen.Store(false)
			// Start from the opposite of what the policy says, so a missed
			// apply shows.
			setBackupShareQuota(map[bool]*bool{true: &off, false: &on}[tc.want])
			rdb := newMiniRedis(t)
			if tc.policy != "" {
				if err := rdb.Set(context.Background(), backupPolicyKey(nodeID), tc.policy, 0).Err(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			watchBackupPolicy(ctx, rdb, nodeID, t.TempDir())
			for _, r := range tc.runs {
				noteRunShareQuota(r)
			}
			if got := backupShareQuota(); got != tc.want {
				t.Fatalf("share = %v, want %v", got, tc.want)
			}
		})
	}
}
