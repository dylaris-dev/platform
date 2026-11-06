package services

import (
	"context"
	"errors"
	"fmt"
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
