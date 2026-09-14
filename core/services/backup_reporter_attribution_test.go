package services

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"
)

// The node -> Core backup channels used to be ONE fleet-wide name each, granted
// to every node by BuildNodeACLRules - tenant BYON machines included. Pub/Sub
// carries no sender identity, so Core took the runId out of the payload and
// wrote backup_runs for whoever asked. Marking a foreign run "success" fires
// enforceRetention, which DELETES that job's older archives from storage.
//
// The channel is now per node token, and this is the second, independent half:
// Core re-derives the owning node from the run and refuses anything that does
// not match. Redis already refuses the publish through the ACL, so a mismatch
// arriving here means an ACL that was never applied or a wider credential -
// which is exactly when a second check has to hold.

type attributionFakeStore struct {
	store.Store

	servers map[int]*models.Server
	nodes   map[int]*models.Node
	srvErr  error
	nodeErr error
}

func (f *attributionFakeStore) GetServerByID(id int) (*models.Server, error) {
	if f.srvErr != nil {
		return nil, f.srvErr
	}
	srv, ok := f.servers[id]
	if !ok {
		return nil, errors.New("no such server")
	}
	return srv, nil
}

func (f *attributionFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	n, ok := f.nodes[id]
	if !ok {
		return nil, errors.New("no such node")
	}
	return n, nil
}

func attributionScheduler() *BackupScheduler {
	return &BackupScheduler{store: &attributionFakeStore{
		servers: map[int]*models.Server{
			7:  {ID: 7, NodeID: 1},
			99: {ID: 99, NodeID: 2},
		},
		nodes: map[int]*models.Node{
			1: {ID: 1, Token: "node-owning"},
			2: {ID: 2, Token: "node-other"},
		},
	}}
}

func TestReporterMatchesServer(t *testing.T) {
	tests := []struct {
		name     string
		channel  string
		serverID int
		want     bool
	}{
		{
			name:     "the hosting node reporting its own run",
			channel:  queue.BackupResultsChannel("node-owning"),
			serverID: 7,
			want:     true,
		},
		{
			name:     "a restore on the hosting node's own channel",
			channel:  queue.BackupRestoresChannel("node-owning"),
			serverID: 7,
			want:     true,
		},
		{
			// The whole point: a tenant's BYON node closing another tenant's run.
			name:     "a different node reporting someone else's run",
			channel:  queue.BackupResultsChannel("node-other"),
			serverID: 7,
			want:     false,
		},
		{
			name:     "a token that belongs to no node at all",
			channel:  queue.BackupResultsChannel("node-never-enrolled"),
			serverID: 7,
			want:     false,
		},
		{
			// The legacy fleet-wide channel name. It carries no token, so it is
			// unattributable and must be dropped rather than waved through.
			name:     "the old fleet-wide channel",
			channel:  "dylaris:backup:results",
			serverID: 7,
			want:     false,
		},
		{
			name:     "an empty token",
			channel:  queue.BackupResultsChannel(""),
			serverID: 7,
			want:     false,
		},
		{
			name:     "a server that cannot be loaded",
			channel:  queue.BackupResultsChannel("node-owning"),
			serverID: 4242,
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := attributionScheduler()
			if got := b.reporterMatchesServer(tc.channel, tc.serverID, "test"); got != tc.want {
				t.Fatalf("reporterMatchesServer(%q, %d) = %v, want %v", tc.channel, tc.serverID, got, tc.want)
			}
		})
	}
}

// A store fault must not be read as "attributable". Losing a result costs one
// run that the reaper later closes as unverified; accepting one lets an
// unattributable message write another tenant's row.
func TestReporterMatchesServerFailsClosedOnAStoreFault(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*attributionFakeStore)
	}{
		{"server lookup fails", func(f *attributionFakeStore) { f.srvErr = errors.New("db down") }},
		{"node lookup fails", func(f *attributionFakeStore) { f.nodeErr = errors.New("db down") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := attributionScheduler()
			tc.mut(b.store.(*attributionFakeStore))
			if b.reporterMatchesServer(queue.BackupResultsChannel("node-owning"), 7, "test") {
				t.Fatal("reporterMatchesServer accepted a message it could not attribute")
			}
		})
	}
}

// The pattern Core subscribes to must match exactly the channels the node
// publishes on, and the extractor must round-trip the token. A drift here is
// silent: nothing is delivered and every run sits "running" until the reaper.
func TestBackupChannelPatternsMatchWhatTheNodePublishes(t *testing.T) {
	for _, tc := range []struct {
		channel string
		want    string
	}{
		{queue.BackupResultsChannel("abc"), "abc"},
		{queue.BackupRestoresChannel("abc"), "abc"},
	} {
		got, ok := queue.NodeTokenFromBackupChannel(tc.channel)
		if !ok || got != tc.want {
			t.Errorf("NodeTokenFromBackupChannel(%q) = (%q, %v), want (%q, true)", tc.channel, got, ok, tc.want)
		}
	}
	if _, ok := queue.NodeTokenFromBackupChannel("dylaris:backup:results"); ok {
		t.Error("the legacy fleet-wide channel yielded a token; it carries none")
	}
	if _, ok := queue.NodeTokenFromBackupChannel("dylaris:server:x:stats:live"); ok {
		t.Error("an unrelated channel yielded a backup token")
	}
}

// consumeFakeStore drives the real consumeResults loop end to end: it answers
// the run -> job -> server -> node chain and records every status write.
//
// Guarded by a mutex because the writes happen on the consumer goroutine while
// the test reads them.
type consumeFakeStore struct {
	attributionFakeStore

	mu       sync.Mutex
	updates  []int // run ids that were actually written
	pruneHit int
}

func (f *consumeFakeStore) GetBackupRun(id int) (*models.BackupRun, error) {
	return &models.BackupRun{ID: id, JobID: 55, Status: "running", StorageKey: "backups/x.tar.gz"}, nil
}
func (f *consumeFakeStore) GetBackupJob(int) (*models.BackupJob, error) {
	return &models.BackupJob{ID: 55, ServerID: 7, RetentionCount: 3}, nil
}

// A filesystem target, so a report is written as the node sent it; what a run on
// object storage is given instead is pinned in backup_report_trust_test.go.
func (f *consumeFakeStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return &models.BackupStorage{ID: 1, Provider: "local"}, nil
}

func (f *consumeFakeStore) UpdateBackupRunStatus(id int, _, _ string, _ int64, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, id)
	return nil
}

// PruneOldBackupRuns must never be reached for a message that was dropped: it
// is the destructive half - a forged "success" on a foreign run prunes that
// job's archive history out of storage.
func (f *consumeFakeStore) PruneOldBackupRuns(int, int) ([]models.BackupRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneHit++
	return nil, nil
}

func (f *consumeFakeStore) seen() ([]int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.updates...), f.pruneHit
}

// TestConsumeResultsDropsAForeignNodesReport wires the real loop, not just the
// helper, because the helper being correct proves nothing about whether the
// call site actually consults it.
//
// The foreign message is followed by a CONTROL message from the hosting node on
// the same subscription. Waiting for the control write to land proves the loop
// was subscribed and had already had its chance at the foreign one, so a zero
// count is a real drop rather than a subscriber that was simply slower than the
// assertion.
func TestConsumeResultsDropsAForeignNodesReport(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	st := &consumeFakeStore{attributionFakeStore: *attributionScheduler().store.(*attributionFakeStore)}
	b := &BackupScheduler{store: st, redis: rdb}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.consumeResults(ctx)

	result := func(runID int, status string) []byte {
		data, _ := json.Marshal(map[string]interface{}{"runId": runID, "status": status, "sizeBytes": 1})
		return data
	}

	// PSUBSCRIBE is asynchronous, so publish until the CONTROL message from the
	// hosting node is observed. Both messages go out on every attempt, in that
	// order, so the foreign one is never published without the control one
	// following it on the same connection.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// The foreign one claims SUCCESS - that is the payload that fires the
		// retention prune. The control one is "failed" so the only thing that can
		// reach PruneOldBackupRuns in this test is the message that must not.
		rdb.Publish(ctx, queue.BackupResultsChannel("node-other"), result(12, "success"))
		rdb.Publish(ctx, queue.BackupResultsChannel("node-owning"), result(34, "failed"))
		time.Sleep(20 * time.Millisecond)
		if ids, _ := st.seen(); len(ids) > 0 {
			break
		}
	}
	ids, pruned := st.seen()
	if len(ids) == 0 {
		t.Fatal("the hosting node's own result never landed; the subscription is not wired to what the node publishes")
	}
	for _, id := range ids {
		if id != 34 {
			t.Fatalf("run %d was written from a foreign node's channel", id)
		}
	}
	if pruned != 0 {
		t.Fatalf("a dropped message still reached the retention prune (%d calls)", pruned)
	}
}
