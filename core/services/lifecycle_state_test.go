package services

import (
	"context"
	"testing"

	"dylaris-core/models"

	"github.com/redis/go-redis/v9"
)

// racingStatusStore lands the node's next status in the mailbox while Core is
// writing the previous one to the database - the gap a GET-then-DEL had.
type racingStatusStore struct {
	statusWatcherFakeStore
	rdb  *redis.Client
	done bool
}

func (f *racingStatusStore) UpdateServerStatus(id int, status string) error {
	if !f.done {
		f.done = true
		f.rdb.Set(context.Background(), "dylaris:server:srv-1:status", "stopped", 0)
	}
	return f.statusWatcherFakeStore.UpdateServerStatus(id, status)
}

// The final "stopped" of a stop is written once and never again. Landing
// between the read and the delete, it was deleted unread and the server sat in
// "stopping" for good.
func TestScanDoesNotDeleteAStatusItHasNotRead(t *testing.T) {
	fs := &racingStatusStore{statusWatcherFakeStore: statusWatcherFakeStore{
		serversByUUID: map[string]models.Server{"srv-1": {ID: 1, UUID: "srv-1", Status: "online"}},
	}}
	svc := newStatusWatcherTest(t, &fs.statusWatcherFakeStore)
	svc.store = fs
	fs.rdb = svc.redis
	ctx := context.Background()
	svc.redis.Set(ctx, "dylaris:server:srv-1:status", "stopping", 0)

	svc.scan()

	got, err := svc.redis.Get(ctx, "dylaris:server:srv-1:status").Result()
	if err != nil || got != "stopped" {
		t.Fatalf("mailbox after the scan = %q (%v), want the node's \"stopped\" kept for the next tick", got, err)
	}
}

// A suspension used to stop only servers whose status read exactly "online".
// One starting, restarting after a crash or finishing an install kept
// desired_state "online" and came back up for a tenant who had been cut off.
func TestStopTenantServersCoversEveryServerMeantToRun(t *testing.T) {
	fs := &stopOrderFakeStore{node: &models.Node{ID: 7, Token: "node-token"}}
	fs.servers = map[string][]models.Server{"u1": {
		{ID: 1, UUID: "a", Status: "online", DesiredState: "online", NodeID: 7},
		{ID: 2, UUID: "b", Status: "starting", DesiredState: "online", NodeID: 7},
		{ID: 3, UUID: "c", Status: "installing", DesiredState: "online", NodeID: 7},
		{ID: 4, UUID: "d", Status: "stopped", DesiredState: "stopped", NodeID: 7},
	}}
	svc := newStopTestService(t, fs)
	rdb := svc.queue.redis
	svc.redis = rdb

	svc.stopTenantServers(context.Background(), "u1")

	want := map[string]bool{"1=stopped": true, "2=stopped": true, "3=stopped": true}
	if len(fs.desiredCalls) != len(want) {
		t.Fatalf("desired_state writes = %v, want servers 1, 2 and 3 stopped", fs.desiredCalls)
	}
	for _, c := range fs.desiredCalls {
		if !want[c] {
			t.Fatalf("unexpected desired_state write %q", c)
		}
	}
	for _, uuid := range []string{"a", "b", "c"} {
		if got, _ := rdb.Get(context.Background(), DesiredStateKey(uuid)).Result(); got != "stopped" {
			t.Errorf("desired_state for %s in Redis = %q, want stopped at once", uuid, got)
		}
	}
	// A stop only where a container can be running.
	if len(fs.statusWrites) != 2 || fs.statusWrites[0] != "1=stopping" || fs.statusWrites[1] != "2=stopping" {
		t.Errorf("status writes = %v, want [1=stopping 2=stopping]", fs.statusWrites)
	}
}
