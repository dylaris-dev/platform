package services

import (
	"context"
	"encoding/json"
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
	m := &RoutingMigrationService{redis: rdb, queue: NewQueueService(rdb), store: statusStore{status: "online"}}
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

// statusStore answers the settle poll with one fixed status.
type statusStore struct {
	store.Store
	status string
}

func (s statusStore) GetServerByUUID(string) (*models.Server, error) {
	return &models.Server{Status: s.status}, nil
}

func redeployFor(t *testing.T, status string) (error, []string) {
	t.Helper()
	oldT, oldP := redeploySettleTimeout, redeployPollEvery
	redeploySettleTimeout, redeployPollEvery = 150*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { redeploySettleTimeout, redeployPollEvery = oldT, oldP })
	rdb := newQueueTestRedis(t)
	m := &RoutingMigrationService{redis: rdb, queue: NewQueueService(rdb), store: statusStore{status: status}}
	err := m.redeployServer(context.Background(), models.Server{UUID: "srv", NodeAddress: "tok", Status: status}, "gateway")
	msgs, _ := rdb.XRange(context.Background(), "dylaris:node:tok:cmds", "-", "+").Result()
	var actions []string
	for _, msg := range msgs {
		data, _ := msg.Values["data"].(string)
		var c struct{ Action string }
		json.Unmarshal([]byte(data), &c)
		actions = append(actions, c.Action)
	}
	return err, actions
}

// A server that did not settle was SIGKILLed and counted as done: a JVM still
// shutting down lost the world since its last save, for nothing, since a
// kill changes no port binding.
func TestRoutingRedeployNeverKills(t *testing.T) {
	err, actions := redeployFor(t, "restarting")
	if err == nil {
		t.Error("a server that never settled counted as done")
	}
	for _, a := range actions {
		if a == "kill" {
			t.Fatalf("the routing switch killed a server: %v", actions)
		}
	}
}

// An install or a move recreates the container itself when it ends; the
// switch recreating it too raced the install's last step.
func TestRoutingRedeployLeavesInstallsAndMovesAlone(t *testing.T) {
	for _, st := range []string{"installing", "migrating", "stopping"} {
		if err, actions := redeployFor(t, st); err != nil || len(actions) != 0 {
			t.Errorf("%s: err %v, commands %v; want none", st, err, actions)
		}
	}
}

// A server held for a full disk is settled: stopped on purpose.
func TestRoutingRedeployTakesAFullDiskAsSettled(t *testing.T) {
	if err, actions := redeployFor(t, "disk_full"); err != nil || len(actions) != 1 || actions[0] != "update_resources" {
		t.Errorf("err %v, commands %v; want one update_resources and no error", err, actions)
	}
}

// The status is read when the server's turn comes, not from the fleet load:
// a move that began since is left alone.
func TestRoutingRedeployReadsTheStatusWhenItsTurnComes(t *testing.T) {
	rdb := newQueueTestRedis(t)
	m := &RoutingMigrationService{redis: rdb, queue: NewQueueService(rdb), store: statusStore{status: "migrating"}}
	if err := m.redeployServer(context.Background(), models.Server{UUID: "srv", NodeAddress: "tok", Status: "online"}, "gateway"); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.XLen(context.Background(), "dylaris:node:tok:cmds").Result(); n != 0 {
		t.Errorf("%d command(s) sent to a server that started moving after the fleet load", n)
	}
}

// A booting server is settled: its container was recreated with the new
// binding, and a modpack can take longer than the wait to finish booting.
func TestRoutingRedeployTakesABootingServerAsSettled(t *testing.T) {
	if err, _ := redeployFor(t, "starting"); err != nil {
		t.Errorf("a booting server counted as failed: %v", err)
	}
}
