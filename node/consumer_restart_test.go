package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"dylaris-pkg/retry"
)

// A consumer that cannot set up its group (Redis away for a moment while the
// node boots) is started again; it used to end the command listener for good,
// leaving a node that heartbeat and never ran another command.
func TestCommandConsumerIsStartedAgain(t *testing.T) {
	old := consumerRestartWait
	consumerRestartWait = func(*retry.Backoff) time.Duration { return time.Millisecond }
	defer func() { consumerRestartWait = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		runCommandConsumer(ctx, func(ctx context.Context) error {
			if runs.Add(1) < 3 {
				return errors.New("NOGROUP: redis unreachable")
			}
			<-ctx.Done()
			return ctx.Err()
		})
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for runs.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("consumer ran %d times, want it started again after each failure", runs.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the restart loop did not end with its context")
	}
}
