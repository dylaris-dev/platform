package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type statsRetryStore struct {
	store.Store
	fail     int
	inserted []models.ServerStatRow
}

func (f *statsRetryStore) GetNodeByToken(string) (*models.Node, error) {
	return &models.Node{ID: 1}, nil
}
func (f *statsRetryStore) ListServersByNode(int) ([]models.Server, error) {
	return []models.Server{{UUID: "srv-1"}}, nil
}
func (f *statsRetryStore) InsertStatsBatch(rows []models.ServerStatRow) error {
	if f.fail > 0 {
		f.fail--
		return errors.New("pgbouncer: server conn crashed")
	}
	f.inserted = append(f.inserted, rows...)
	return nil
}

// J51: a batch whose insert failed was left un-ACKed "so we retry", but the
// consumer only ever read ">", which never hands an entry out again.
func TestAFailedStatsBatchIsRetried(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	const key = "dylaris:node:tok:stats:batch"
	st := &statsRetryStore{fail: 1}
	s := NewStatsConsumerService(st, rdb, "core-1")
	r := &statsStreamReader{key: key, group: "dylaris-core-stats", consumer: "core-1", nodeToken: "tok", block: 10 * time.Millisecond}
	rdb.XGroupCreateMkStream(ctx, key, r.group, "0")
	add := func(ts int64) {
		rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]interface{}{
			"data": fmt.Sprintf(`{"ts":%d,"stats":[{"uuid":"srv-1","cpu":1}]}`, ts),
		}})
	}

	add(100)
	s.readStats(ctx, r) // the insert fails
	if len(st.inserted) != 0 {
		t.Fatal("inserted although the store failed")
	}
	add(200)
	s.readStats(ctx, r) // new entries still flow while the retry waits
	r.retryAt = time.Now()
	s.readStats(ctx, r) // the retry is due

	if len(st.inserted) != 2 {
		t.Fatalf("inserted %d rows, want both batches", len(st.inserted))
	}
	if !r.retryAt.IsZero() {
		t.Error("the retry stayed armed with nothing pending")
	}
	if p, _ := rdb.XPending(ctx, key, r.group).Result(); p.Count != 0 {
		t.Errorf("%d entries still pending", p.Count)
	}
}

// A batch that keeps failing is dropped once it is older than the window,
// rather than retried forever.
func TestAStatsBatchPastItsWindowIsDropped(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	const key = "dylaris:node:tok:stats:batch"
	st := &statsRetryStore{}
	s := NewStatsConsumerService(st, rdb, "core-1")
	r := &statsStreamReader{key: key, group: "dylaris-core-stats", consumer: "core-1", nodeToken: "tok", block: 10 * time.Millisecond}
	rdb.XGroupCreateMkStream(ctx, key, r.group, "0")
	old := time.Now().Add(-2 * statsPendingMaxAge).UnixMilli()
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", old), Values: map[string]interface{}{
		"data": `{"ts":1,"stats":[{"uuid":"srv-1"}]}`,
	}})

	s.readStats(ctx, r)
	if len(st.inserted) != 0 {
		t.Errorf("inserted a batch past its window: %+v", st.inserted)
	}
	if p, _ := rdb.XPending(ctx, key, r.group).Result(); p.Count != 0 {
		t.Errorf("%d entries still pending", p.Count)
	}
}

type statsGoneStore struct{ store.Store }

func (statsGoneStore) GetNodeByToken(string) (*models.Node, error) { return nil, sql.ErrNoRows }

// K44: a deleted node's consumer polled for ever, and its NOGROUP handler
// recreated the stream the teardown had just removed.
func TestStatsConsumerOfADeletedNodeStops(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	const key = "dylaris:node:gone:stats:batch"
	s := NewStatsConsumerService(statsGoneStore{}, rdb, "core-1")
	s.nodeKeys[key] = true

	done := make(chan struct{})
	go func() { s.consumeStream(key); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer of a deleted node kept running")
	}
	if mr.Exists(key) {
		t.Error("the consumer recreated the deleted node's stream")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodeKeys[key] {
		t.Error("the stream stayed registered, so it is never consumed again")
	}

	// A read error on a deleted node's stream must not recreate it either.
	r := &statsStreamReader{key: key, group: "dylaris-core-stats", consumer: "core-1", nodeToken: "gone", block: 10 * time.Millisecond}
	if s.readStats(context.Background(), r) {
		t.Error("readStats kept going for a deleted node")
	}
	if mr.Exists(key) {
		t.Error("the NOGROUP handler recreated the deleted node's stream")
	}
}

func TestNodeRedisKeysCoverTheStatsStreams(t *testing.T) {
	for _, want := range []string{"dylaris:node:tok:stats:batch", "dylaris:node:tok:system"} {
		if !slices.Contains(NodeRedisKeys("tok"), want) {
			t.Errorf("%s outlives its node", want)
		}
	}
}

// L40: the consumer name is the Core's hostname, a new one on every redeploy.
// A batch the old Core had read but not ACKed sat in ITS pending list, which
// the new Core's "0" read never returns, so it was lost.
func TestABatchLeftByAPreviousCoreIsClaimed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	const key = "dylaris:node:tok:stats:batch"
	st := &statsRetryStore{}
	s := NewStatsConsumerService(st, rdb, "core-new")
	r := &statsStreamReader{key: key, group: "dylaris-core-stats", consumer: "core-new", nodeToken: "tok", block: 10 * time.Millisecond}
	rdb.XGroupCreateMkStream(ctx, key, r.group, "0")
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]interface{}{
		"data": `{"ts":100,"stats":[{"uuid":"srv-1","cpu":1}]}`,
	}})
	// The old Core read it and went away before the ACK.
	rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: r.group, Consumer: "core-old", Streams: []string{key, ">"}, Count: 10})
	mr.SetTime(time.Now().Add(2 * statsRetryEvery))

	s.readStats(ctx, r)
	if r.retryAt.IsZero() && len(st.inserted) == 0 {
		t.Fatal("nothing claimed from the old consumer")
	}
	s.readStats(ctx, r)
	if len(st.inserted) != 1 {
		t.Fatalf("inserted %d rows, want the old Core's batch", len(st.inserted))
	}
	if p, _ := rdb.XPending(ctx, key, r.group).Result(); p.Count != 0 {
		t.Errorf("%d entries still pending", p.Count)
	}
}

type statsDBDownStore struct{ statsRetryStore }

func (*statsDBDownStore) ListServersByNode(int) ([]models.Server, error) {
	return nil, errors.New("pgbouncer: no server connection")
}

// L42: with the database down from the start, the allowed set was never
// loaded, every batch produced zero rows, and zero rows were ACKed as done.
func TestABatchIsNotAckedWhileTheServerListIsUnknown(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	const key = "dylaris:node:tok:stats:batch"
	st := &statsDBDownStore{}
	s := NewStatsConsumerService(st, rdb, "core-1")
	r := &statsStreamReader{key: key, group: "dylaris-core-stats", consumer: "core-1", nodeToken: "tok", block: 10 * time.Millisecond}
	rdb.XGroupCreateMkStream(ctx, key, r.group, "0")
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]interface{}{
		"data": `{"ts":100,"stats":[{"uuid":"srv-1","cpu":1}]}`,
	}})

	s.readStats(ctx, r)
	if p, _ := rdb.XPending(ctx, key, r.group).Result(); p.Count != 1 {
		t.Fatalf("%d entries pending, want the batch kept for a retry", p.Count)
	}
	if r.retryAt.IsZero() {
		t.Error("no retry scheduled")
	}
}
