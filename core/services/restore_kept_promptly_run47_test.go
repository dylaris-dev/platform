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

func (f *restoreResultStore) ListBackupRestores(serverID, limit int) ([]models.BackupRestore, error) {
	return append([]models.BackupRestore{f.restore}, f.others...), nil
}

// A kept result was read only for restores requested six hours ago, and then
// reconciled the install, mod and sub-server rows against everything changed
// since, a newer restore included (R47-01, R47-02). It is read on every tick
// now, and one that is no longer fresh only closes the row.
func TestAKeptRestoreResultIsReadPromptlyAndAppliedLateOnlyAsStatus(t *testing.T) {
	overlay := false
	now := time.Now()
	kept := func(ts int64) string {
		return fmt.Sprintf(`{"restoreId":1,"status":"success","error":"","timestamp":%d}`, ts)
	}
	tests := []struct {
		name         string
		requested    time.Duration // ago
		kept         string
		rowStatus    string
		others       []models.BackupRestore
		wantUpdated  int
		wantReplaced int
		wantClosed   bool
	}{
		{name: "fresh result on a minute-old restore", requested: time.Minute, kept: kept(now.Add(-30 * time.Second).Unix()), wantUpdated: 1, wantReplaced: 1},
		{name: "fresh result on a restore past the deadline", requested: 7 * time.Hour, kept: kept(now.Unix()), wantUpdated: 1, wantReplaced: 1},
		{name: "late result: status only", requested: time.Hour, kept: kept(now.Add(-10 * time.Minute).Unix()), wantUpdated: 1},
		{name: "no timestamp counts as late", requested: time.Hour, kept: `{"restoreId":1,"status":"success"}`, wantUpdated: 1},
		{name: "a newer restore has finished: status only", requested: time.Hour, kept: kept(now.Unix()),
			others: []models.BackupRestore{{ID: 2, ServerID: 2, Status: "success"}}, wantUpdated: 1},
		{name: "a newer restore still queued does not count", requested: time.Hour, kept: kept(now.Unix()),
			others: []models.BackupRestore{{ID: 2, ServerID: 2, Status: "queued"}}, wantUpdated: 1, wantReplaced: 1},
		{name: "an older finished restore does not count", requested: time.Hour, kept: kept(now.Unix()),
			others: []models.BackupRestore{{ID: 0, ServerID: 2, Status: "success"}}, wantUpdated: 1, wantReplaced: 1},
		{name: "row already final is never re-applied", requested: time.Hour, kept: kept(now.Unix()), rowStatus: "failed"},
		{name: "young open row without a result is left alone", requested: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			key := queue.RestoreResultKey("u", 1)
			if tt.kept != "" {
				mr.Set(key, tt.kept)
			}
			status := tt.rowStatus
			if status == "" {
				status = "queued"
			}
			st := &restoreResultStore{
				restoreReapStore: restoreReapStore{open: []models.AbandonedBackupRestore{
					{ID: 1, ServerUUID: "u", NodeID: 1, RequestedAt: now.Add(-tt.requested)},
				}},
				restore: models.BackupRestore{ID: 1, RunID: 5, ServerID: 2, Status: status, Overlay: &overlay},
				run:     models.BackupRun{ID: 5, Manifest: `{"scope":"main","mods":[{"subServer":"main","mods":[]}]}`},
				others:  tt.others,
			}
			b := &BackupScheduler{store: st, redis: rdb, nodeConnected: func(int) bool { return true },
				restoreNodeSeen: map[int]time.Time{1: now.Add(-time.Hour)}, restoreSeenPass: now.Add(-time.Minute)}
			// Two passes: an applied result must not be applied again.
			b.reapAbandonedRestores(context.Background(), now)
			b.reapAbandonedRestores(context.Background(), now.Add(time.Minute))

			if len(st.updated) != tt.wantUpdated {
				t.Errorf("row written %v, want %d times", st.updated, tt.wantUpdated)
			}
			if len(st.replaced) != tt.wantReplaced {
				t.Errorf("mod rows replaced for %v, want %d", st.replaced, tt.wantReplaced)
			}
			if got := len(st.closed) > 0; got != tt.wantClosed {
				t.Errorf("closed %v, want closed=%v", st.closed, tt.wantClosed)
			}
			if tt.wantUpdated > 0 && mr.Exists(key) {
				t.Errorf("the applied result is still kept")
			}
		})
	}
}
