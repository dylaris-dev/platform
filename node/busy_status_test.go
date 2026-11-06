package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newBusyRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// TestHoldBusySurvivesTheStatusWatcherDrain is the property this helper exists
// for, and the one its first version got wrong.
//
// Core's status watcher GETs the status key and DELETEs it every 5 seconds - it
// is a mailbox, not a state field. A marker written only there is therefore gone
// within 5s no matter how often it is refreshed, which is exactly what a live
// reinstall showed: the key alternated between the value and absent, and the
// reconciler restarted the container in one of the gaps. The interlock has to
// live somewhere nothing else drains.
func TestHoldBusySurvivesTheStatusWatcherDrain(t *testing.T) {
	rdb, _ := newBusyRedis(t)
	ctx := context.Background()
	release := holdBusyStatus(rdb, "srv-uuid", "installing", busyStatusTTL)
	defer release()

	// Simulate the watcher: drain the status key the way scan() does.
	rdb.Del(ctx, "dylaris:server:srv-uuid:status")

	if isNodeBusy(ctx, rdb, "srv-uuid") == false {
		t.Fatal("the reconciler would treat this server as unprotected right after Core drained the status key")
	}
}

// TestIsNodeBusyFalseWhenNothingIsRunning: the interlock must not be sticky, or
// a server would stay unreconciled after the work finished.
func TestIsNodeBusyFalseWhenNothingIsRunning(t *testing.T) {
	rdb, _ := newBusyRedis(t)
	ctx := context.Background()
	if isNodeBusy(ctx, rdb, "never-touched") {
		t.Fatal("isNodeBusy is true for a server no operation ever ran on")
	}
	release := holdBusyStatus(rdb, "srv-uuid", "installing", busyStatusTTL)
	release()
	if isNodeBusy(ctx, rdb, "srv-uuid") {
		t.Fatal("isNodeBusy is still true after release; the reconciler would never look after this server again")
	}
}

// TestHoldBusyReportsAProtectedStatusToo: the status key write is what the panel
// renders, and it must use a word the reconciler's own map agrees with so the
// two guards cannot disagree during the seconds before Core drains it.
func TestHoldBusyReportsAProtectedStatusToo(t *testing.T) {
	rdb, _ := newBusyRedis(t)
	release := holdBusyStatus(rdb, "srv-uuid", "installing", busyStatusTTL)
	defer release()

	got, err := rdb.Get(context.Background(), "dylaris:server:srv-uuid:status").Result()
	if err != nil {
		t.Fatalf("status key not set: %v", err)
	}
	if !protectedStatuses[got] {
		t.Fatalf("holdBusyStatus reports %q, which is not in protectedStatuses", got)
	}
}

// TestHoldBusyOutlivesItsTTL: the marker's TTL is far shorter than the work it
// covers. A Paper reinstall measured 10s, but Forge and NeoForge run their
// installer in a one-shot container and take minutes, so a set-once marker would
// expire mid-install and hand the container back to the reconciler.
func TestHoldBusyOutlivesItsTTL(t *testing.T) {
	rdb, mr := newBusyRedis(t)
	ctx := context.Background()
	// Short TTL so the wall-clock refresh ticker (ttl/3) fires within the test.
	// miniredis expiry advances only via FastForward, so the two clocks are
	// driven separately: jump most of a TTL, then give the ticker real time.
	const ttl = 300 * time.Millisecond
	release := holdBusyStatus(rdb, "srv-uuid", "installing", ttl)
	defer release()

	for range 5 {
		mr.FastForward(ttl * 2 / 3)
		time.Sleep(ttl/3 + 40*time.Millisecond)
	}
	if !isNodeBusy(ctx, rdb, "srv-uuid") {
		t.Fatal("the interlock expired during the work it was meant to cover")
	}

	// Control: without the refresh the same elapsed time DOES expire it, so the
	// assertion above is about the ticker and not about miniredis being lenient.
	rdb.Set(ctx, nodeBusyKey("other"), "installing", ttl)
	mr.FastForward(ttl * 2)
	if isNodeBusy(ctx, rdb, "other") {
		t.Fatal("a set-once marker survived past its TTL; this test cannot prove the refresh works")
	}
}

// The key format is a cross-component contract: Core reads this key to tell an
// install nobody is working on apart from one still running
// (handlers.annotateStalledInstalls). There is a copy of the format on each
// side, so each side pins it - renaming it here alone would silently disable
// Core's detection with nothing failing anywhere.
func TestNodeBusyKeyFormatIsAContract(t *testing.T) {
	if got := nodeBusyKey("abc"); got != "dylaris:server:abc:node_busy" {
		t.Errorf("nodeBusyKey = %q; Core reads this exact format", got)
	}
}

// A refresh already past its select when the work ends must not land after the
// release: node_busy then stayed up to a TTL with nothing behind it, holding the
// reconciler off and making Core refuse power actions with a 409.
func TestHoldBusyReleaseWaitsForAnInFlightRefresh(t *testing.T) {
	rdb, _ := newBusyRedis(t)
	ctx := context.Background()
	for i := range 300 {
		release := holdBusyStatus(rdb, "srv-uuid", "installing", 3*time.Millisecond)
		time.Sleep(time.Duration(i%7) * 300 * time.Microsecond)
		release()
		if isNodeBusy(ctx, rdb, "srv-uuid") {
			t.Fatalf("round %d: node_busy still set after release", i)
		}
	}
}
