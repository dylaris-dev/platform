package queue

import (
	"context"
	"testing"
	"time"
)

// An express message runs while every main worker is busy and other work
// waits: a node's kill sat behind hours of backups once all eight slots were
// taken. Main-pool work still runs, once a worker is free.
func TestExpressRunsPastABusyPool(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.Block = 50 * time.Millisecond
	c.Concurrency = 1
	c.Express = func(p []byte) bool { return string(p) == "kill" }
	c.ExpressConcurrency = 1
	// Before publishing: Run creates the group at "$", past anything already there.
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	release := make(chan struct{})
	slowStarted := make(chan struct{})
	ran := make(chan string, 4)
	handler := func(_ context.Context, data []byte) error {
		if string(data) == "backup" {
			close(slowStarted)
			<-release
		}
		ran <- string(data)
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Run(runCtx, handler); close(done) }()

	for _, p := range []string{"backup", "start", "kill"} {
		if _, err := Publish(ctx, rdb, "q", []byte(p)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if p == "backup" {
			select {
			case <-slowStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("the long job never started")
			}
		}
	}
	select {
	case got := <-ran:
		if got != "kill" {
			t.Fatalf("%q ran first, want the kill past the busy pool", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the kill waited behind the long job")
	}
	close(release)
	want := map[string]bool{"backup": true, "start": true}
	for len(want) > 0 {
		select {
		case got := <-ran:
			delete(want, got)
		case <-time.After(3 * time.Second):
			t.Fatalf("main-pool work never ran: %v", want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not end with its context")
	}
}

// Shutting down while main-pool work waits for a worker does not panic on a
// closed channel and does not hang.
func TestExpressShutdownWithWaitingWork(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.Block = 50 * time.Millisecond
	c.Concurrency = 1
	c.Express = func(p []byte) bool { return false }
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	handler := func(hctx context.Context, data []byte) error {
		started <- struct{}{}
		select {
		case <-release:
		case <-hctx.Done():
		}
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Run(runCtx, handler); close(done) }()
	for i := 0; i < 5; i++ {
		if _, err := Publish(ctx, rdb, "q", []byte("job")); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	<-started
	time.Sleep(200 * time.Millisecond) // the rest are read and waiting
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung on shutdown with work waiting")
	}
	close(release)
}

// Main-pool messages read ahead while the pool is busy still run in stream
// order: a goroutine each let the scheduler pick, and back-to-back commands for
// one server ran the wrong way round.
func TestExpressKeepsMainPoolOrder(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.Block = 50 * time.Millisecond
	c.Concurrency = 1
	c.Express = func(p []byte) bool { return false }
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	ran := make(chan string, 16)
	handler := func(_ context.Context, data []byte) error {
		if string(data) == "first" {
			close(started)
			<-release
		}
		ran <- string(data)
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(runCtx, handler)
	if _, err := Publish(ctx, rdb, "q", []byte("first")); err != nil {
		t.Fatal(err)
	}
	<-started
	want := []string{"first", "m1", "m2", "m3", "m4", "m5", "m6"}
	for _, p := range want[1:] {
		if _, err := Publish(ctx, rdb, "q", []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // all read ahead while "first" runs
	close(release)
	for i, w := range want {
		select {
		case got := <-ran:
			if got != w {
				t.Fatalf("position %d ran %q, want %q", i, got, w)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d ran", i)
		}
	}
}
