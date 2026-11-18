package leader

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// One failed refresh does not end the leadership. The lease is still ours in
// Redis until its TTL runs out, so stepping down at once left the platform with
// no leader at all until it did: this instance and every follower failed SETNX
// against the key it still held, and leader-gated Pub/Sub results were dropped.
func TestALeaseRefreshErrorKeepsLeadingWhileTheLeaseIsValid(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	l := New(rdb, "test:leader", "core-1")
	l.ttl, l.refreshEvery, l.retryEvery, l.safety = 1200*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond, 400*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)
	defer l.Stop()

	waitFor(t, "the lease", time.Second, l.IsLeader)

	// A short Redis hiccup, well inside the lease.
	mr.SetError("LOADING")
	time.Sleep(150 * time.Millisecond)
	if !l.IsLeader() {
		t.Fatal("stepped down on a refresh error while the lease was still valid")
	}
	mr.SetError("")
	time.Sleep(150 * time.Millisecond)
	if !l.IsLeader() {
		t.Fatal("lost the leadership after Redis recovered inside the lease")
	}

	// An outage longer than the lease ends it before the TTL could.
	mr.SetError("LOADING")
	waitFor(t, "the step-down", 3*time.Second, func() bool { return !l.IsLeader() })
}
