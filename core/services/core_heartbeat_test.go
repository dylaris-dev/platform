package services

import (
	"context"
	"testing"
	"time"
)

// waitForStop runs Stop and fails the test if it has not returned in time,
// rather than letting a regression hang the whole package until the go test
// timeout. Stop blocks on the heartbeat goroutine by design, so "it returns"
// is the property under test in several cases below.
func waitForStop(t *testing.T, s *CoreHeartbeatService) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Stop(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}
}

// TestCoreHeartbeat_StopRemovesThisCoreFromTheOnlineSet is the behaviour the
// storage settings depend on: the key carries a 30s TTL, so a Core that shuts
// down without deleting it stays counted as online long enough to keep the
// host-path backend unsavable after a deployment is scaled back to one Core.
// Asserted through OnlineCoreIDs rather than a raw key read, so the writer and
// the reader are pinned together.
func TestCoreHeartbeat_StopRemovesThisCoreFromTheOnlineSet(t *testing.T) {
	rdb, _ := newCoreInstancesRedis(t)
	svc := NewCoreHeartbeatService(rdb, "core-a", "default", "2026.08.28", 25501)

	svc.Start()

	ids, err := OnlineCoreIDs(context.Background(), rdb)
	if err != nil {
		t.Fatalf("OnlineCoreIDs after Start: %v", err)
	}
	if len(ids) != 1 || ids[0] != "core-a" {
		t.Fatalf("online after Start = %v, want [core-a]", ids)
	}

	waitForStop(t, svc)

	ids, err = OnlineCoreIDs(context.Background(), rdb)
	if err != nil {
		t.Fatalf("OnlineCoreIDs after Stop: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("online after Stop = %v, want none (the key must be deleted, not left to expire)", ids)
	}
}

// TestCoreHeartbeat_StopWithoutStartReturns covers the shutdown path of a Core
// that failed before it got as far as starting its heartbeat. Stop waits on the
// heartbeat goroutine, and that goroutine was never launched, so an unguarded
// Stop would block the whole shutdown sequence forever.
func TestCoreHeartbeat_StopWithoutStartReturns(t *testing.T) {
	rdb, _ := newCoreInstancesRedis(t)
	svc := NewCoreHeartbeatService(rdb, "core-a", "default", "2026.08.28", 25501)

	waitForStop(t, svc)
}

// TestCoreHeartbeat_StopTwiceReturns pins idempotence. Stop closes a channel to
// signal the loop, and closing an already-closed channel panics, so a second
// Stop must take the already-stopped branch.
func TestCoreHeartbeat_StopTwiceReturns(t *testing.T) {
	rdb, _ := newCoreInstancesRedis(t)
	svc := NewCoreHeartbeatService(rdb, "core-a", "default", "2026.08.28", 25501)

	svc.Start()
	waitForStop(t, svc)
	waitForStop(t, svc)
}

// TestCoreHeartbeat_StartTwiceDoesNotLaunchASecondLoop pins the other half of
// the same channel discipline: each loop closes doneCh when it exits, so two
// loops would close it twice and panic. Stop is what makes the second loop
// observable, since that is when the close happens.
func TestCoreHeartbeat_StartTwiceDoesNotLaunchASecondLoop(t *testing.T) {
	rdb, _ := newCoreInstancesRedis(t)
	svc := NewCoreHeartbeatService(rdb, "core-a", "default", "2026.08.28", 25501)

	svc.Start()
	svc.Start()
	waitForStop(t, svc)
}

// TestCoreHeartbeat_StopSurvivesAnUnreachableRedis keeps shutdown moving when
// the thing it is trying to clean up is exactly what is broken. The key expires
// on its own in that case, so failing to delete it is survivable - hanging or
// panicking on the way out is not.
func TestCoreHeartbeat_StopSurvivesAnUnreachableRedis(t *testing.T) {
	rdb, mr := newCoreInstancesRedis(t)
	svc := NewCoreHeartbeatService(rdb, "core-a", "default", "2026.08.28", 25501)

	svc.Start()
	mr.Close()

	waitForStop(t, svc)
}

// The node finds the Cores from this index: its Redis login no longer may
// SCAN, which lists every key NAME on the platform whatever its key patterns
// say. A Core that died without its Stop leaves a stale id, and every live
// Core sweeps those out, since a node cannot.
func TestCoreHeartbeat_KeepsTheCoreIndex(t *testing.T) {
	rdb, mr := newCoreInstancesRedis(t)
	mr.SAdd(CoreIndexKey, "core-dead") // its heartbeat key expired long ago
	svc := NewCoreHeartbeatService(rdb, "core-a", "default", "2026.10.03", 25501)
	svc.Start()

	members, _ := mr.Members(CoreIndexKey)
	if len(members) != 1 || members[0] != "core-a" {
		t.Fatalf("index = %v, want [core-a] (listed, and the dead id swept)", members)
	}

	waitForStop(t, svc)
	if members, _ := mr.Members(CoreIndexKey); len(members) != 0 {
		t.Fatalf("index after Stop = %v, want empty", members)
	}
}
