package services

import (
	"context"
	"testing"

	"dylaris-core/models"
)

type desiredCall struct {
	id    int
	state string
}

// reconcileFailedFakeStore adds desired_state recording to the shared fake.
type reconcileFailedFakeStore struct {
	statusWatcherFakeStore
	desiredCalls []desiredCall
}

func (f *reconcileFailedFakeStore) UpdateServerDesiredState(id int, state string) error {
	f.desiredCalls = append(f.desiredCalls, desiredCall{id, state})
	return nil
}

func newReconcileFailedTest(t *testing.T, fs *reconcileFailedFakeStore) *StatusWatcherService {
	t.Helper()
	return &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
}

// TestConsumeReconcileFailures_LandsTheNodesGiveUp closes a signal that was
// written and never read.
//
// When a container crashes past its retry limit the node writes
// reconcile_failed, with the comment "Set failed key so core can surface it" -
// and no code outside the node read that key. The server kept the last status
// the reconciler published, "restarting", forever: the one state that needs the
// owner to act looked like the one state that resolves itself.
func TestConsumeReconcileFailures_LandsTheNodesGiveUp(t *testing.T) {
	fs := &reconcileFailedFakeStore{}
	fs.serversByUUID = map[string]models.Server{
		"srv-1": {ID: 1, UUID: "srv-1", Status: "restarting", DesiredState: "online"},
	}
	svc := newReconcileFailedTest(t, fs)
	ctx := context.Background()
	svc.redis.Set(ctx, "dylaris:server:srv-1:reconcile_failed", "Container crashed 5 times, auto-restart disabled", 0)

	if !svc.consumeReconcileFailures(ctx) {
		t.Fatal("reported no change for a server the node had given up on")
	}
	if len(fs.statusCalls) != 1 || fs.statusCalls[0] != (schedStatusCall{1, "offline"}) {
		t.Errorf("status calls = %+v, want one write of offline", fs.statusCalls)
	}
	// The intent to run it has to go too, or the next reconciler pass with a
	// fresh tracker starts the crash cycle over again.
	if len(fs.desiredCalls) != 1 || fs.desiredCalls[0] != (desiredCall{1, "stopped"}) {
		t.Errorf("desired_state calls = %+v, want one write of stopped", fs.desiredCalls)
	}
}

// TestConsumeReconcileFailures_IsIdempotent: the key stays until the node clears
// it on a deliberate start, so this runs against it every 5s. It must land once
// and then leave the row alone, or it would fight a start the moment one lands.
func TestConsumeReconcileFailures_IsIdempotent(t *testing.T) {
	fs := &reconcileFailedFakeStore{}
	fs.serversByUUID = map[string]models.Server{
		"srv-1": {ID: 1, UUID: "srv-1", Status: "offline", DesiredState: "stopped"},
	}
	svc := newReconcileFailedTest(t, fs)
	ctx := context.Background()
	svc.redis.Set(ctx, "dylaris:server:srv-1:reconcile_failed", "Container crashed 5 times", 0)

	if svc.consumeReconcileFailures(ctx) {
		t.Error("reported a change for a server already marked offline/stopped")
	}
	if len(fs.statusCalls) != 0 || len(fs.desiredCalls) != 0 {
		t.Errorf("rewrote an already-landed decision: status=%+v desired=%+v", fs.statusCalls, fs.desiredCalls)
	}
}

// TestConsumeReconcileFailures_IgnoresServersWithoutTheKey is the negative
// control: a healthy server must not be touched by this pass.
func TestConsumeReconcileFailures_IgnoresServersWithoutTheKey(t *testing.T) {
	fs := &reconcileFailedFakeStore{}
	fs.serversByUUID = map[string]models.Server{
		"srv-1": {ID: 1, UUID: "srv-1", Status: "online", DesiredState: "online"},
	}
	svc := newReconcileFailedTest(t, fs)

	if svc.consumeReconcileFailures(context.Background()) {
		t.Error("reported a change with no reconcile_failed key present")
	}
	if len(fs.statusCalls) != 0 || len(fs.desiredCalls) != 0 {
		t.Errorf("touched a healthy server: status=%+v desired=%+v", fs.statusCalls, fs.desiredCalls)
	}
}

// "stop" typed in the console used to be restarted as a crash. The node now
// marks a server that stopped itself, and Core lands it as STOPPED - not
// offline, nothing failed - and stops intending to run it. The marker stays
// for the node, which clears it on the next start.
func TestConsumeNodeHold_LandsAServerThatStoppedItself(t *testing.T) {
	fs := &reconcileFailedFakeStore{}
	fs.serversByUUID = map[string]models.Server{
		"srv-1": {ID: 1, UUID: "srv-1", Status: "restarting", DesiredState: "online"},
	}
	svc := newReconcileFailedTest(t, fs)
	ctx := context.Background()
	svc.redis.Set(ctx, "dylaris:server:srv-1:stopped_by_server", "1", 0)

	// Through the scan tick, so a scan that stops consulting the marker fails.
	svc.scan()
	if len(fs.statusCalls) != 1 || fs.statusCalls[0] != (schedStatusCall{1, "stopped"}) {
		t.Errorf("status calls = %+v, want one write of stopped", fs.statusCalls)
	}
	if len(fs.desiredCalls) != 1 || fs.desiredCalls[0] != (desiredCall{1, "stopped"}) {
		t.Errorf("desired_state calls = %+v, want one write of stopped", fs.desiredCalls)
	}
	if n, _ := svc.redis.Exists(ctx, "dylaris:server:srv-1:stopped_by_server").Result(); n != 1 {
		t.Error("Core removed the marker; the node clears it on the next start")
	}
}

// A move or an install owns the status while it runs. A server stopped from the
// console keeps its marker until it next starts, so landing it then wrote
// "stopped" over the move every scan.
func TestConsumeNodeHold_LeavesAMoveAlone(t *testing.T) {
	for _, status := range []string{"migrating", "installing"} {
		fs := &reconcileFailedFakeStore{}
		fs.serversByUUID = map[string]models.Server{"srv-1": {ID: 1, UUID: "srv-1", Status: status, DesiredState: "online"}}
		svc := newReconcileFailedTest(t, fs)
		ctx := context.Background()
		svc.redis.Set(ctx, "dylaris:server:srv-1:stopped_by_server", "1", 0)
		if svc.consumeNodeHold(ctx, "stopped_by_server", "stopped", "it stopped itself") || len(fs.statusCalls) != 0 {
			t.Fatalf("%s: overwritten with %+v", status, fs.statusCalls)
		}
	}
	fs := &reconcileFailedFakeStore{}
	fs.serversByUUID = map[string]models.Server{"srv-1": {ID: 1, UUID: "srv-1", Status: "stopped", DesiredState: "online"}}
	svc := newReconcileFailedTest(t, fs)
	ctx := context.Background()
	svc.redis.Set(ctx, "dylaris:server:srv-1:stopped_by_server", "1", 0)
	svc.redis.Set(ctx, NodeBusyKey("srv-1"), "installing", 0)
	if svc.consumeNodeHold(ctx, "stopped_by_server", "stopped", "it stopped itself") {
		t.Fatal("landed while the node was working on the server")
	}
}
