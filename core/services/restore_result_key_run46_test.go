package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type restoreResultStore struct {
	restoreReapStore
	restore  models.BackupRestore
	others   []models.BackupRestore
	run      models.BackupRun
	updated  []string
	replaced []string
	// updateFails makes the next that many row writes fail.
	updateFails int
	// beforeUpdate runs once, at the next row write, before it.
	beforeUpdate func()
}

func (f *restoreResultStore) GetBackupRestore(id int) (*models.BackupRestore, error) {
	r := f.restore
	return &r, nil
}

// Models the query: only an open row changes, or a failed one turning success.
func (f *restoreResultStore) UpdateBackupRestoreStatus(id int, status, msg string, completed time.Time) (bool, error) {
	if hook := f.beforeUpdate; hook != nil {
		f.beforeUpdate = nil
		hook()
	}
	if f.updateFails > 0 {
		f.updateFails--
		return false, fmt.Errorf("db blip")
	}
	if s := f.restore.Status; s == "queued" || s == "running" || (s == "failed" && status == "success") {
		f.restore.Status = status
		f.updated = append(f.updated, status+":"+msg)
		return true, nil
	}
	return false, nil
}

func (f *restoreResultStore) GetBackupRun(id int) (*models.BackupRun, error) {
	r := f.run
	return &r, nil
}

func (f *restoreResultStore) ReplaceServerMods(serverID int, sub string, mods []models.ServerMod) error {
	f.replaced = append(f.replaced, sub)
	return nil
}

// A result published while no Core held the leader lease was lost, and the
// reaper closed a restore that had replaced the world as failed, with the mod
// and install rows never reconciled (R46-02). A result the node also kept is
// applied the way the live consumer applies it.
func TestTheReaperAppliesAKeptRestoreResult(t *testing.T) {
	overlay := false
	tests := []struct {
		name         string
		kept         string // "" = no key, an old node
		wantClosed   bool
		wantUpdated  []string
		wantReplaced int
	}{
		{name: "old node, nothing kept", wantClosed: true},
		{name: "kept success", kept: fmt.Sprintf(`{"restoreId":1,"status":"success","error":"","timestamp":%d}`, time.Now().Unix()), wantUpdated: []string{"success:"}, wantReplaced: 1},
		{name: "kept failure", kept: `{"restoreId":1,"status":"failed","error":"disk full"}`, wantUpdated: []string{"failed:disk full"}},
		{name: "key names another restore", kept: `{"restoreId":9,"status":"success"}`, wantClosed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			key := queue.RestoreResultKey("u", 1)
			if tt.kept != "" {
				mr.Set(key, tt.kept)
				mr.SetTTL(key, queue.RestoreResultTTL)
			}
			now := time.Now()
			st := &restoreResultStore{
				restoreReapStore: restoreReapStore{open: []models.AbandonedBackupRestore{
					{ID: 1, ServerUUID: "u", NodeID: 1, RequestedAt: now.Add(-7 * time.Hour)},
				}},
				restore: models.BackupRestore{ID: 1, RunID: 5, ServerID: 2, Status: "queued", Overlay: &overlay},
				run:     models.BackupRun{ID: 5, Manifest: `{"scope":"main","mods":[{"subServer":"main","mods":[]}]}`},
			}
			b := &BackupScheduler{store: st, redis: rdb, nodeConnected: func(int) bool { return true },
				restoreNodeSeen: map[int]time.Time{1: now.Add(-time.Hour)}, restoreSeenPass: now.Add(-time.Minute)}
			b.reapAbandonedRestores(context.Background(), now)

			if got := len(st.closed) == 1; got != tt.wantClosed {
				t.Errorf("closed %v, want closed=%v", st.closed, tt.wantClosed)
			}
			if len(st.updated) != len(tt.wantUpdated) || (len(st.updated) > 0 && st.updated[0] != tt.wantUpdated[0]) {
				t.Errorf("updated %v, want %v", st.updated, tt.wantUpdated)
			}
			if len(st.replaced) != tt.wantReplaced {
				t.Errorf("mod rows replaced for %v, want %d sub-servers", st.replaced, tt.wantReplaced)
			}
			if tt.wantUpdated != nil && mr.Exists(key) {
				t.Errorf("the applied result is still kept, a later pass would apply it again")
			}
		})
	}
}
