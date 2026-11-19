package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// R30-02: a sub-server delete waits up to hours for a backup, an install and
// a power action of its server. It did that inside one of the consumer's
// eight workers, so a few deletes during a backup stalled every tenant of the
// node. It now waits beside the pool, one per server and sub-server.
func TestASubServerDeleteDoesNotHoldACommandWorker(t *testing.T) {
	del := func(sub string) NodeCommand {
		c := NodeCommand{Action: "delete_sub_server"}
		c.Config.UUID, c.Config.ActiveSubServer = "srv-r30", sub
		return c
	}
	release := make(chan struct{})
	var ran atomic.Int32
	blocking := func(context.Context, NodeCommand) { ran.Add(1); <-release }

	for _, tc := range []struct {
		name    string
		cmd     NodeCommand
		wantRun int32
	}{
		{"the delete is handed off and the worker returns", del("survival"), 1},
		{"the same delete again while the first waits is dropped", del("survival"), 1},
		{"another sub-server of the server waits on its own", del("creative"), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() { runCommand(context.Background(), tc.cmd, blocking); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("runCommand waited for the delete inside the worker")
			}
			deadline := time.Now().Add(2 * time.Second)
			for ran.Load() != tc.wantRun && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := ran.Load(); got != tc.wantRun {
				t.Fatalf("deletes running = %d, want %d", got, tc.wantRun)
			}
		})
	}
	close(release)

	// Once it has run, the same delete may be sent again.
	deadline := time.Now().Add(2 * time.Second)
	for commandsInFlight.has("delete_sub_server:srv-r30/survival") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if commandsInFlight.has("delete_sub_server:srv-r30/survival") {
		t.Fatal("a finished delete still blocks the next one")
	}

	// A panic beside the pool must not take the node down: the consumer's own
	// recover does not reach this goroutine.
	panicked := make(chan struct{})
	runCommand(context.Background(), del("boom"), func(context.Context, NodeCommand) {
		defer close(panicked)
		panic("boom")
	})
	<-panicked

	// Every other command still runs in the worker, before it is acked.
	inline := false
	runCommand(context.Background(), NodeCommand{Action: "start"}, func(context.Context, NodeCommand) { inline = true })
	if !inline {
		t.Fatal("a start no longer ran inside its worker")
	}
}
