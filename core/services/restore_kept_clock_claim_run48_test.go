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

// attributedRestoreStore lets the live consumer's attribution pass: server 2 is
// hosted by the node whose token is "tok".
type attributedRestoreStore struct{ *restoreResultStore }

func (f attributedRestoreStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, NodeID: 1}, nil
}

func (f attributedRestoreStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "tok"}, nil
}

func newRun48Scheduler(t *testing.T, others []models.BackupRestore) (*BackupScheduler, *restoreResultStore, *miniredis.Miniredis) {
	t.Helper()
	overlay := false
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	now := time.Now()
	st := &restoreResultStore{
		restoreReapStore: restoreReapStore{open: []models.AbandonedBackupRestore{
			{ID: 1, ServerUUID: "u", NodeID: 1, RequestedAt: now.Add(-time.Hour)},
		}},
		restore: models.BackupRestore{ID: 1, RunID: 5, ServerID: 2, Status: "queued", Overlay: &overlay},
		run:     models.BackupRun{ID: 5, Manifest: `{"scope":"main","mods":[{"subServer":"main","mods":[]}]}`},
		others:  others,
	}
	b := &BackupScheduler{store: attributedRestoreStore{st}, redis: rdb, nodeConnected: func(int) bool { return true },
		restoreNodeSeen: map[int]time.Time{1: now.Add(-time.Hour)}, restoreSeenPass: now.Add(-time.Minute)}
	return b, st, mr
}

func run48Payload(ts int64) string {
	return fmt.Sprintf(`{"restoreId":1,"status":"success","error":"","serverUuid":"u","timestamp":%d}`, ts)
}

// A kept result's age came from the node's own timestamp against Core's clock,
// so a node clock behind Core downgraded a fresh result to status only, and one
// ahead made a result kept through an outage look fresh (R48-01). The age is
// the TTL the node set minus what is left of it, all on the Redis clock. A
// newer restore that FAILED never touched the server and does not overtake the
// result (R48-02/R48-03).
func TestAKeptRestoreResultIsAgedOnTheRedisClock(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		nodeClock    time.Duration // node timestamp relative to Core's now
		age          time.Duration // RestoreResultTTL - remaining TTL; <0 = no TTL
		ttl          time.Duration // explicit TTL, overrides age
		others       []models.BackupRestore
		wantReplaced int
	}{
		{name: "fresh, node clock on time", age: 30 * time.Second, wantReplaced: 1},
		{name: "fresh, node clock 10 minutes behind", nodeClock: -10 * time.Minute, age: 30 * time.Second, wantReplaced: 1},
		{name: "fresh, node clock 5 hours ahead", nodeClock: 5 * time.Hour, age: 30 * time.Second, wantReplaced: 1},
		{name: "late, node clock 5 hours ahead", nodeClock: 5 * time.Hour, age: 3 * time.Hour},
		{name: "late, node clock on time", age: 10 * time.Minute},
		{name: "no TTL counts as late", age: -1},
		{name: "a TTL longer than the node sets counts as late", ttl: queue.RestoreResultTTL + time.Hour},
		{name: "a newer failed restore does not count", age: 30 * time.Second,
			others: []models.BackupRestore{{ID: 2, ServerID: 2, Status: "failed"}}, wantReplaced: 1},
		{name: "a newer successful restore counts", age: 30 * time.Second,
			others: []models.BackupRestore{{ID: 2, ServerID: 2, Status: "success"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, st, mr := newRun48Scheduler(t, tt.others)
			key := queue.RestoreResultKey("u", 1)
			mr.Set(key, run48Payload(now.Add(tt.nodeClock).Unix()))
			switch {
			case tt.ttl != 0:
				mr.SetTTL(key, tt.ttl)
			case tt.age >= 0:
				mr.SetTTL(key, queue.RestoreResultTTL-tt.age)
			}
			b.reapAbandonedRestores(context.Background(), now)

			if len(st.updated) != 1 {
				t.Errorf("row written %v, want once", st.updated)
			}
			if len(st.replaced) != tt.wantReplaced {
				t.Errorf("mod rows replaced for %v, want %d", st.replaced, tt.wantReplaced)
			}
			if mr.Exists(key) {
				t.Errorf("the applied result is still kept")
			}
		})
	}
}

// The tick and the live consumer both read the kept result: the node SETs it
// before the PUBLISH, so a tick in between applied it and the live consumer
// applied it again (R48-02). Each claims the key first; only the winner applies.
func TestTheTickAndTheLiveConsumerApplyAResultOnce(t *testing.T) {
	now := time.Now()
	channel := queue.BackupRestoresChannel("tok")
	tests := []struct {
		name  string
		order []string
	}{
		{name: "live consumer only", order: []string{"live"}},
		{name: "tick first", order: []string{"tick", "live"}},
		{name: "live consumer first", order: []string{"live", "tick"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, st, mr := newRun48Scheduler(t, nil)
			key := queue.RestoreResultKey("u", 1)
			payload := run48Payload(now.Unix())
			mr.Set(key, payload)
			mr.SetTTL(key, queue.RestoreResultTTL-time.Second)
			for _, step := range tt.order {
				if step == "tick" {
					b.reapAbandonedRestores(context.Background(), now)
				} else {
					b.handleRestoreResult(context.Background(), channel, payload)
				}
			}
			if len(st.updated) != 1 {
				t.Errorf("row written %v, want once", st.updated)
			}
			if len(st.replaced) != 1 {
				t.Errorf("mod rows replaced for %v, want a full reconciliation once", st.replaced)
			}
			if mr.Exists(key) {
				t.Errorf("the applied result is still kept")
			}
		})
	}
}

// An older node keeps nothing, so its live result is applied without a claim.
func TestALiveResultFromANodeThatKeepsNothingIsApplied(t *testing.T) {
	b, st, _ := newRun48Scheduler(t, nil)
	b.handleRestoreResult(context.Background(), queue.BackupRestoresChannel("tok"), `{"restoreId":1,"status":"success","error":""}`)
	if len(st.updated) != 1 || len(st.replaced) != 1 {
		t.Errorf("updated %v replaced %v, want one full apply", st.updated, st.replaced)
	}
}
