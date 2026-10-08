package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// readNew pulls the next batch of new messages for the consumer (test helper).
func readNew(t *testing.T, c *Consumer) []redis.XMessage {
	t.Helper()
	res, err := c.rdb.XReadGroup(context.Background(), &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.name,
		Streams:  []string{c.stream, ">"},
		Count:    10,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		t.Fatalf("XReadGroup: %v", err)
	}
	var out []redis.XMessage
	for _, st := range res {
		out = append(out, st.Messages...)
	}
	return out
}

func pendingCount(t *testing.T, c *Consumer) int64 {
	t.Helper()
	p, err := c.rdb.XPending(context.Background(), c.stream, c.group).Result()
	if err != nil {
		t.Fatalf("XPending: %v", err)
	}
	return p.Count
}

func TestPublishConsumeRoundTrip(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.Block = 50 * time.Millisecond
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := make(chan string, 1)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(runCtx, func(_ context.Context, data []byte) error { got <- string(data); return nil })

	select {
	case v := <-got:
		if v != "hello" {
			t.Fatalf("got %q, want hello", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestRetryThenAckOnRecovery(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("x")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	calls := 0
	failOnce := func(_ context.Context, _ []byte) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return nil
	}

	// First delivery: handler fails -> message left pending, not acked.
	for _, m := range readNew(t, c) {
		c.handleOne(ctx, m, failOnce)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if pendingCount(t, c) != 1 {
		t.Fatalf("pending=%d, want 1 after failure", pendingCount(t, c))
	}

	// Recovery reprocesses own pending; handler now succeeds -> acked.
	for _, m := range c.ownPending(ctx) {
		c.handleOne(ctx, m, failOnce)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 after recovery", calls)
	}
	if pendingCount(t, c) != 0 {
		t.Fatalf("pending=%d, want 0 after successful retry", pendingCount(t, c))
	}
}

func TestDedupSkipsAlreadyProcessed(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("y")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msgs := readNew(t, c)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]

	// Simulate "processed but ACK lost": the done-marker exists on redelivery.
	rdb.Set(ctx, c.doneKey(m.ID), "1", time.Hour)

	called := false
	c.handleOne(ctx, m, func(_ context.Context, _ []byte) error { called = true; return nil })
	if called {
		t.Fatal("handler ran for an already-processed message")
	}
	if pendingCount(t, c) != 0 {
		t.Fatalf("pending=%d, want 0 (dedup path must ACK)", pendingCount(t, c))
	}
}

func TestDeadLetterAfterMaxDeliveries(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.MaxDeliveries = 2
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("poison")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msgs := readNew(t, c)
	m := msgs[0]
	alwaysFail := func(_ context.Context, _ []byte) error { return errors.New("nope") }

	c.handleOne(ctx, m, alwaysFail) // attempt 1 -> pending
	if pendingCount(t, c) != 1 {
		t.Fatalf("pending=%d, want 1 after first failure", pendingCount(t, c))
	}
	c.handleOne(ctx, m, alwaysFail) // attempt 2 == MaxDeliveries -> dead-letter + ack

	if pendingCount(t, c) != 0 {
		t.Fatalf("pending=%d, want 0 after dead-letter", pendingCount(t, c))
	}
	n, err := rdb.XLen(ctx, c.deadKey()).Result()
	if err != nil {
		t.Fatalf("XLen dead: %v", err)
	}
	if n != 1 {
		t.Fatalf("dead-letter len=%d, want 1", n)
	}
}

// Parking a poison message can itself fail, and the ACK must not happen when it
// does. Before this was checked, a failed XAdd still ACKed: the payload was
// destroyed with no copy anywhere, at exactly the moment an operator most needs
// to look at it.
//
// The failure is injected by giving the dead-letter key the wrong Redis type, so
// XADD returns WRONGTYPE while every other command on the connection keeps
// working - closing the server would fail the ACK too and prove nothing.
func TestDeadLetter_DoesNotAckWhenParkingFails(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.MaxDeliveries = 1
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := rdb.Set(ctx, c.deadKey(), "not-a-stream", 0).Err(); err != nil {
		t.Fatalf("seed wrong-type dead key: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("poison")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	m := readNew(t, c)[0]
	c.handleOne(ctx, m, func(_ context.Context, _ []byte) error { return errors.New("nope") })

	if got := pendingCount(t, c); got != 1 {
		t.Fatalf("pending=%d, want 1 - the message was ACKed even though it was never parked, "+
			"so its payload is gone", got)
	}

	// And once the dead-letter stream is usable again, the retry parks it.
	if err := rdb.Del(ctx, c.deadKey()).Err(); err != nil {
		t.Fatalf("clear dead key: %v", err)
	}
	c.handleOne(ctx, m, func(_ context.Context, _ []byte) error { return errors.New("nope") })
	if got := pendingCount(t, c); got != 0 {
		t.Errorf("pending=%d, want 0 - the retry should have parked and acked it", got)
	}
	n, err := rdb.XLen(ctx, c.deadKey()).Result()
	if err != nil {
		t.Fatalf("XLen dead: %v", err)
	}
	if n != 1 {
		t.Errorf("dead-letter len=%d, want 1", n)
	}
}

// The unparseable path dead-letters BEFORE the handler runs, so it has no
// attempts counter to fall back on. It must still refuse to ACK a message it
// could not park.
func TestDeadLetter_UnparseableIsNotDroppedWhenParkingFails(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := rdb.Set(ctx, c.deadKey(), "not-a-stream", 0).Err(); err != nil {
		t.Fatalf("seed wrong-type dead key: %v", err)
	}
	// No "data" field: this is what msgData rejects.
	if err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: "q", Values: map[string]interface{}{"nodata": "x"},
	}).Err(); err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	m := readNew(t, c)[0]
	c.handleOne(ctx, m, func(_ context.Context, _ []byte) error { return nil })

	if got := pendingCount(t, c); got != 1 {
		t.Errorf("pending=%d, want 1 - an unparseable message was dropped without being parked", got)
	}
}

// TestSlowHandlerIsNotRunTwiceByRecovery is the regression guard for a
// concurrent duplicate execution found live on a node.
//
// Recovery lists this consumer's PENDING entries, and pending covers both "the
// worker died" and "the worker is still busy" - Redis cannot tell them apart.
// The dedup marker cannot either: it is written after the handler returns, so a
// handler still running looks identical to one that never ran. A node "stop"
// takes longer than Block (it sends save-all, waits, then gives the server up to
// 30s), so the read timed out mid-handler, the idle tick called recoverPending,
// and the same stop ran a second time in parallel. The second one's own deadline
// then SIGKILLed a Minecraft server the first was shutting down cleanly.
func TestSlowHandlerIsNotRunTwiceByRecovery(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	// Block far shorter than the handler takes, which is the whole point: the
	// read must time out while the handler is still working, exactly as a 5s
	// Block does against a stop that runs for tens of seconds.
	c.Block = 50 * time.Millisecond
	// A free worker, as on a node (8): with one, a duplicate could only queue
	// behind the original and would never show.
	c.Concurrency = 2
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("slow")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var mu sync.Mutex
	starts := 0
	done := make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(runCtx, func(_ context.Context, _ []byte) error {
		mu.Lock()
		starts++
		first := starts == 1
		mu.Unlock()
		if first {
			time.Sleep(600 * time.Millisecond) // spans several Block timeouts
			close(done)
		}
		return nil
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
	// Give the loop a few more idle ticks to try to re-deliver it.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Fatalf("handler ran %d times, want 1: recovery re-delivered an entry that was still being processed", starts)
	}
}

// The other half of this guarantee - that the in-flight mark is RELEASED, so a
// handler that failed on purpose is still retried by the next recovery pass - is
// already covered by TestRetryThenAckOnRecovery above, which now also exercises
// beginInflight/endInflight across its two handleOne calls.

// TestRunSelfHealsAfterGroupVanishes is the regression guard for a NOGROUP loop
// found live. Redis runs with no persistence (save and appendonly both off), so a
// restart wipes the stream AND its consumer group. Run creates the group once at
// start; after the wipe every XREADGROUP returned "NOGROUP ..." forever and the
// node silently stopped receiving Core commands until the process was restarted.
// Run must recreate the group at "0" so a command enqueued AFTER the restart is
// still delivered (the "$" startup position would skip it). FlushAll models the
// restart faithfully: stream + group + done-markers all gone at once.
func TestRunSelfHealsAfterGroupVanishes(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.Block = 50 * time.Millisecond
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	got := make(chan string, 1)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(runCtx, func(_ context.Context, data []byte) error { got <- string(data); return nil })

	// Let Run enter its read loop with the group in place.
	time.Sleep(150 * time.Millisecond)

	// Redis restart with no persistence: the next XREADGROUP hits NOGROUP.
	if err := rdb.FlushAll(ctx).Err(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	// Core enqueues a command after the restart (XADD recreates the stream, but
	// no group exists). Only a self-heal that recreates the group at "0" delivers it.
	if _, err := Publish(ctx, rdb, "q", []byte("post-restart")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case v := <-got:
		if v != "post-restart" {
			t.Fatalf("got %q, want post-restart", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command enqueued after a Redis restart was never delivered: the consumer did not self-heal the vanished group")
	}
}

// TestPanickingHandlerIsDeadLetteredNotFatal is the guard for the one poison
// message this package's own doc promises it can survive.
//
// "A message that keeps failing is dead-lettered after MaxDeliveries so it can't
// block the queue" held only for a handler that RETURNS an error. A handler that
// PANICS took the whole process with it: Run is called from a bare goroutine in
// both consumers (platform/node/main.go and the migration orchestrator), so
// nothing above it recovers. The message stayed pending, the next start
// redelivered it through recoverPending, and it panicked again - and because the
// attempts counter only advances on a returned error, MaxDeliveries never fired.
// One malformed command wedged a customer's node permanently, through every
// restart, with no way out but deleting the stream by hand.
//
// The same shape was already found and fixed once in this audit, on the hub's
// queue consumer (a [:8] slice on a short token). There the panic cost one
// process; here the recovery loop makes it cost every restart after it too.
func TestPanickingHandlerIsDeadLetteredNotFatal(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	c := NewConsumer(rdb, "q", "g", "c1")
	c.MaxDeliveries = 2
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := Publish(ctx, rdb, "q", []byte("poison")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msgs := readNew(t, c)
	m := msgs[0]
	// A log-prefix slice on a payload shorter than the code assumes: the exact
	// shape found in the hub's queue consumer earlier in this audit, not a bare
	// panic(). The payload is 6 bytes.
	alwaysPanic := func(_ context.Context, data []byte) error {
		_ = string(data[:64])
		return nil
	}

	// If handleOne does not recover, this call takes the test binary down and the
	// failure reads as a crash rather than an assertion - which is exactly what it
	// does to the node.
	c.handleOne(ctx, m, alwaysPanic) // attempt 1 -> pending
	if got := pendingCount(t, c); got != 1 {
		t.Fatalf("pending=%d, want 1 after the first panic (a panic must count as a failed attempt)", got)
	}
	c.handleOne(ctx, m, alwaysPanic) // attempt 2 == MaxDeliveries -> dead-letter + ack

	if got := pendingCount(t, c); got != 0 {
		t.Fatalf("pending=%d, want 0: a panicking message is never dead-lettered, so it is redelivered forever", got)
	}
	n, err := rdb.XLen(ctx, c.deadKey()).Result()
	if err != nil {
		t.Fatalf("XLen dead: %v", err)
	}
	if n != 1 {
		t.Fatalf("dead-letter len=%d, want 1", n)
	}
}
