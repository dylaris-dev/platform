package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// None of the targeted methods below (writeStatus/readStatus/GetStatus/
// updateProgress/releaseLock) touch RoutingMigrationService.store or .queue
// (verified in source), so no fake store is needed here - a bare struct
// literal with only .redis set is enough.

func TestRoutingMigrationService_WriteStatus_ReadStatus_RoundTrip(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	m := &RoutingMigrationService{redis: rdb}

	want := MigrationStatus{Running: true, Total: 10, Done: 3, Failed: 1}
	m.writeStatus(ctx, want)

	if got := m.readStatus(ctx); got != want {
		t.Errorf("readStatus = %+v, want %+v", got, want)
	}
	if got := m.GetStatus(ctx); got != want {
		t.Errorf("GetStatus = %+v, want %+v", got, want)
	}
}

func TestRoutingMigrationService_ReadStatus_MissingKey_ZeroValue(t *testing.T) {
	rdb := newQueueTestRedis(t)
	m := &RoutingMigrationService{redis: rdb}

	if got := m.readStatus(context.Background()); got != (MigrationStatus{}) {
		t.Errorf("readStatus on missing key = %+v, want zero value", got)
	}
}

func TestRoutingMigrationService_ReadStatus_MalformedJSON_ZeroValue(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	m := &RoutingMigrationService{redis: rdb}

	if err := rdb.Set(ctx, routingMigrationKey, "not-json", 0).Err(); err != nil {
		t.Fatalf("seed malformed status: %v", err)
	}

	if got := m.readStatus(ctx); got != (MigrationStatus{}) {
		t.Errorf("readStatus on malformed JSON = %+v, want zero value", got)
	}
}

func TestRoutingMigrationService_UpdateProgress(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	m := &RoutingMigrationService{redis: rdb}

	m.writeStatus(ctx, MigrationStatus{Running: true, Total: 5})
	m.updateProgress(ctx, 2, 1, 5)

	want := MigrationStatus{Running: true, Total: 5, Done: 2, Failed: 1}
	if got := m.readStatus(ctx); got != want {
		t.Errorf("updateProgress result = %+v, want %+v (read-modify-write over the existing status)", got, want)
	}
}

func TestRoutingMigrationService_ReleaseLock(t *testing.T) {
	t.Run("deletes the lock key", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		m := &RoutingMigrationService{redis: rdb}

		if err := rdb.Set(ctx, routingMigrationLockKey, "1", 0).Err(); err != nil {
			t.Fatalf("seed lock key: %v", err)
		}
		if err := m.releaseLock(ctx); err != nil {
			t.Fatalf("releaseLock: %v", err)
		}
		if n, _ := rdb.Exists(ctx, routingMigrationLockKey).Result(); n != 0 {
			t.Error("expected the lock key to be deleted")
		}
	})

	t.Run("nil redis is a safe no-op", func(t *testing.T) {
		m := &RoutingMigrationService{}
		if err := m.releaseLock(context.Background()); err != nil {
			t.Errorf("releaseLock with nil redis = %v, want nil (no panic)", err)
		}
	})
}

// failingServerStore fails exactly the fleet load. Everything else is
// unreachable in this test.
type failingServerStore struct {
	store.Store
}

func (failingServerStore) GetAllActiveServers() ([]models.Server, error) {
	return nil, errors.New("connection reset while streaming rows")
}

// Run must propagate a failed fleet load, and it must give the lock back so a
// retry is possible. This is what makes the routing-mode handler's
// migrationError branch reachable: before GetAllActiveServers checked
// rows.Err(), a cut stream produced an EMPTY list and a nil error instead, so
// Run returned (0, nil) - "nothing to migrate" - and the handler answered
// success while every server stayed on the old routing.
func TestRoutingMigrationRunPropagatesAFailedFleetLoad(t *testing.T) {
	rdb := newQueueTestRedis(t)
	m := &RoutingMigrationService{redis: rdb, store: failingServerStore{}}

	n, err := m.Run(context.Background(), "gateway")
	if err == nil {
		t.Fatalf("Run = (%d, nil); a fleet that could not be loaded must not read as nothing to migrate", n)
	}
	if n != 0 {
		t.Errorf("queued = %d, want 0", n)
	}
	// A held lock would block every retry until the TTL expired.
	free, err := rdb.SetNX(context.Background(), routingMigrationLockKey, "1", time.Minute).Result()
	if err != nil {
		t.Fatalf("SetNX: %v", err)
	}
	if !free {
		t.Error("the migration lock was not released after the failed load; every retry would be refused until its TTL expired")
	}
}

// A routing-mode switch recreates every container; it sent no cpuset, so every
// pinned server came back unpinned.
func TestRoutingRedeployKeepsTheCpuset(t *testing.T) {
	rdb := newQueueTestRedis(t)
	m := &RoutingMigrationService{redis: rdb, queue: NewQueueService(rdb), store: failingServerStore{}}
	ctx, cancel := context.WithCancel(context.Background())
	srv := models.Server{UUID: "srv-pin", NodeAddress: "node-tok", Cpuset: "2-3", Memory: 2048}
	done := make(chan struct{})
	go func() { m.redeployServer(ctx, srv, "gateway"); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := rdb.XRange(context.Background(), "dylaris:node:node-tok:cmds", "-", "+").Result()
		if len(msgs) > 0 {
			data, _ := msgs[0].Values["data"].(string)
			if !strings.Contains(data, `"cpusetCpus":"2-3"`) {
				t.Fatalf("update_resources carried no cpuset: %s", data)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no command was sent")
}
