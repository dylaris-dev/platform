package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type restoreReapStore struct {
	store.Store
	open      []models.AbandonedBackupRestore
	gotCutoff time.Time
	closed    []int
}

func (f *restoreReapStore) ListAbandonedBackupRestores(before time.Time, limit int) ([]models.AbandonedBackupRestore, error) {
	f.gotCutoff = before
	return f.open, nil
}

func (f *restoreReapStore) CloseAbandonedBackupRestore(id int, message string, completed time.Time) (bool, error) {
	f.closed = append(f.closed, id)
	return true, nil
}

// Only the node's Pub/Sub result closed a restore, so one lost on the way (a
// Core without a leader, a node that was deleted) showed "queued" for good.
// A restore the node is still running holds the server's busy key and is
// left alone, however long it takes.
func TestAnAbandonedRestoreIsClosedButARunningOneIsNot(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	mr.Set("dylaris:server:busy-uuid:node_busy", "restarting")

	now := time.Now()
	st := &restoreReapStore{open: []models.AbandonedBackupRestore{
		{ID: 1, ServerUUID: "lost-uuid", NodeID: 1, RequestedAt: now.Add(-7 * time.Hour)},
		{ID: 2, ServerUUID: "busy-uuid", NodeID: 1, RequestedAt: now.Add(-7 * time.Hour)},
		// Offline: it still holds the command and runs it when it returns,
		// from local storage without asking Core, so failing the row would
		// be a lie the node then overwrites a world under.
		{ID: 3, ServerUUID: "offline-uuid", NodeID: 2, RequestedAt: now.Add(-7 * time.Hour)},
	}}
	b := &BackupScheduler{store: st, redis: rdb, nodeConnected: func(id int) bool { return id == 1 },
		// Reachable for longer than the settle time already.
		restoreNodeSeen: map[int]time.Time{1: now.Add(-time.Hour)}, restoreSeenPass: now.Add(-time.Minute)}
	b.reapAbandonedRestores(context.Background(), now)

	if len(st.closed) != 1 || st.closed[0] != 1 {
		t.Fatalf("closed %v, want only the restore a connected node is not running", st.closed)
	}
	if want := now.Add(-backupRunAbandonedAfter); !st.gotCutoff.Equal(want) {
		t.Errorf("cutoff %v, want %v", st.gotCutoff, want)
	}
}

func (f *restoreReapStore) ListDueBackupJobs(time.Time) ([]models.BackupJob, error) { return nil, nil }
func (f *restoreReapStore) ListAbandonedBackupRuns(time.Time, int) ([]models.BackupRun, error) {
	return nil, nil
}

// The scheduler's tick is what runs the reaper.
func TestTheSchedulerTickReapsRestores(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	st := &restoreReapStore{open: []models.AbandonedBackupRestore{{ID: 7, ServerUUID: "u", RequestedAt: time.Now().Add(-7 * time.Hour)}}}
	(&BackupScheduler{store: st, redis: rdb, nodeConnected: func(int) bool { return true },
		restoreNodeSeen: map[int]time.Time{0: time.Now().Add(-time.Hour)}, restoreSeenPass: time.Now().Add(-time.Minute)}).tick(context.Background())
	if len(st.closed) != 1 {
		t.Fatalf("the tick closed %v", st.closed)
	}
}

// A node that has just reconnected has not read its queue: no busy key says it
// is restoring yet. The restore is closed only once the node has stayed
// reachable past the settle time.
func TestARestoreIsNotReapedRightAfterItsNodeReconnects(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	now := time.Now()
	st := &restoreReapStore{open: []models.AbandonedBackupRestore{{ID: 1, ServerUUID: "u", NodeID: 1, RequestedAt: now.Add(-7 * time.Hour)}}}
	b := &BackupScheduler{store: st, redis: rdb, nodeConnected: func(int) bool { return true }}
	// One pass a minute, as the scheduler ticks.
	for m := time.Duration(0); m < restoreReapSettle; m += time.Minute {
		b.reapAbandonedRestores(context.Background(), now.Add(m))
	}
	if len(st.closed) != 0 {
		t.Fatalf("closed %v right after the node reconnected", st.closed)
	}
	b.reapAbandonedRestores(context.Background(), now.Add(restoreReapSettle))
	if len(st.closed) != 1 {
		t.Fatalf("closed %v once the node had settled, want the restore", st.closed)
	}
}

// A first-seen time left from an earlier stretch - this replica was not the
// leader for a while - must not make a node that just reconnected look settled.
func TestAStaleFirstSeenDoesNotSkipTheWait(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	now := time.Now()
	st := &restoreReapStore{open: []models.AbandonedBackupRestore{{ID: 1, ServerUUID: "u", NodeID: 1, RequestedAt: now.Add(-7 * time.Hour)}}}
	b := &BackupScheduler{store: st, redis: rdb, nodeConnected: func(int) bool { return true },
		restoreNodeSeen: map[int]time.Time{1: now.Add(-3 * time.Hour)}, restoreSeenPass: now.Add(-2 * time.Hour)}
	b.reapAbandonedRestores(context.Background(), now)
	if len(st.closed) != 0 {
		t.Fatalf("closed %v on a stale first-seen time", st.closed)
	}
}
