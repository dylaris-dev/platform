package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// recoveredSlowThenLive leaves "slow" pending from a previous life, starts Run
// with the given concurrency, then publishes "fast". It returns whether "fast"
// ran while the recovered "slow" was still running, and the most handlers that
// ever ran at once.
func recoveredSlowThenLive(t *testing.T, concurrency int) (fastDuringSlow bool, peak int32) {
	t.Helper()
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.Block = 50 * time.Millisecond
	c.Concurrency = concurrency
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("slow")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if n := len(readNew(t, c)); n != 1 { // delivered, never acked: a crash mid-work
		t.Fatalf("read %d", n)
	}

	release := make(chan struct{})
	slowStarted := make(chan struct{})
	fastDone := make(chan struct{})
	var running, top int32
	handler := func(_ context.Context, data []byte) error {
		n := atomic.AddInt32(&running, 1)
		defer atomic.AddInt32(&running, -1)
		for {
			old := atomic.LoadInt32(&top)
			if n <= old || atomic.CompareAndSwapInt32(&top, old, n) {
				break
			}
		}
		if string(data) == "slow" {
			close(slowStarted)
			<-release
			return nil
		}
		close(fastDone)
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(runCtx, handler)

	select {
	case <-slowStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the pending entry was not recovered")
	}
	if _, err := Publish(ctx, rdb, "q", []byte("fast")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-fastDone:
		fastDuringSlow = true
	case <-time.After(time.Second):
	}
	close(release)
	select {
	case <-fastDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the live entry never ran")
	}
	return fastDuringSlow, atomic.LoadInt32(&top)
}

// A recovered entry ran in the reading goroutine, so a node restarted during a
// backup read no new command until that backup finished - hours, for every
// tenant on the node.
func TestARecoveredEntryDoesNotStopTheConsumerReading(t *testing.T) {
	if during, _ := recoveredSlowThenLive(t, 2); !during {
		t.Fatal("a live entry waited for a recovered one with a worker free")
	}
}

// And it ran BESIDE the pool: an idle tick retried a failed entry in the
// reading goroutine while a worker was busy, so a Concurrency of 1 (the
// migration orchestrator) ran two at once.
func TestARetryCountsAgainstConcurrency(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.Block = 20 * time.Millisecond
	c.Concurrency = 1
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("retry")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	release := make(chan struct{})
	failed := make(chan struct{})
	slowStarted := make(chan struct{})
	retried := make(chan struct{})
	var running, top, retryCalls int32
	handler := func(_ context.Context, data []byte) error {
		n := atomic.AddInt32(&running, 1)
		defer atomic.AddInt32(&running, -1)
		if n > atomic.LoadInt32(&top) {
			atomic.StoreInt32(&top, n)
		}
		switch string(data) {
		case "slow":
			close(slowStarted)
			<-release
		case "retry":
			if atomic.AddInt32(&retryCalls, 1) == 1 {
				// "slow" is queued before this fails, so it is read before
				// the first idle tick can retry this one.
				if _, err := Publish(ctx, rdb, "q", []byte("slow")); err != nil {
					t.Errorf("Publish: %v", err)
				}
				close(failed)
				return errors.New("first attempt fails")
			}
			if atomic.LoadInt32(&retryCalls) == 2 {
				time.Sleep(50 * time.Millisecond) // long enough to overlap "slow" if it runs beside it
				close(retried)
			}
		}
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(runCtx, handler)

	<-failed
	<-slowStarted
	time.Sleep(300 * time.Millisecond) // many idle ticks while "slow" holds the only worker
	close(release)
	select {
	case <-retried:
	case <-time.After(3 * time.Second):
		t.Fatal("the failed entry was never retried")
	}
	if peak := atomic.LoadInt32(&top); peak != 1 {
		t.Fatalf("%d handlers ran at once with Concurrency 1", peak)
	}
}
