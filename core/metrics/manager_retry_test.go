package metrics

import (
	"context"
	"testing"
)

// Opening the statistics database used to be a single attempt at boot.
//
// Measured in production on 2026-09-03: one Core replica started 51 seconds
// before `metricsdb` was resolvable, logged "metrics: disabled" once, and
// recorded nothing for the life of the process. Sampling is leader-gated, and
// that replica held the lease - so the whole cluster stopped recording while
// the other replica sat on a working connection doing nothing. Nothing failed,
// nothing alerted, and the only visible symptom was an empty chart.
//
// needsRetry is the decision that fixes it, tested here rather than through the
// ticker so the states are checkable without waiting on wall-clock time.
func TestAnUnreachableTargetIsRetried(t *testing.T) {
	m := NewManager(t.Context())

	// Before boot has chosen a target there is nothing to reopen. Retrying here
	// would mean this loop deciding a policy it does not own.
	if _, retry := m.needsRetry(); retry {
		t.Fatal("a manager that was never applied wants a retry")
	}

	// A failed Apply must leave the target BEHIND. This is the whole fix: the
	// old code kept only what had opened successfully, so a failure left
	// nothing to try again with.
	if err := m.Apply("host='127.0.0.1' port='1' user='x' dbname='x' sslmode='disable'"); err == nil {
		t.Fatal("opening a dead address succeeded")
	}
	dsn, retry := m.needsRetry()
	if !retry {
		t.Fatal("a target that failed to open is not retried")
	}
	if dsn == "" {
		t.Fatal("the retry has no target to reopen")
	}
	if m.Handle() != nil {
		t.Fatal("a failed open left a handle behind")
	}
}

// Close is deliberate - shutdown, or an operator switching recording off. The
// retry loop must not read it as an outage and reopen what was just shut down.
func TestCloseIsNotAnOutage(t *testing.T) {
	m := NewManager(t.Context())
	_ = m.Apply("host='127.0.0.1' port='1' user='x' dbname='x' sslmode='disable'")
	if _, retry := m.needsRetry(); !retry {
		t.Fatal("precondition: the failed target should be pending a retry")
	}
	m.Close()
	if _, retry := m.needsRetry(); retry {
		t.Fatal("a closed manager reopens itself")
	}
}

// Every method here tolerates a nil receiver, and the retry decision is no
// exception - a nil Manager is the normal state wherever metrics are optional.
func TestNeedsRetryOnANilManager(t *testing.T) {
	var m *Manager
	if m.DSN() != "" || m.Handle() != nil || m.Recorder() != nil {
		t.Fatal("a nil manager answered as if it held something")
	}
	if err := m.Apply(""); err == nil {
		t.Fatal("Apply on a nil manager reported success")
	}
	m.Close() // must not panic
	// Watch has to return rather than spin: NewManager starts one per manager,
	// and a nil receiver reaching it would otherwise be a busy goroutine.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m.Watch(ctx)
}

// An empty target is not an outage either: it is what an installation with no
// statistics database looks like, and what switching recording off leaves
// behind. Retrying it would mean opening nothing, forever, every 15 seconds.
func TestNoTargetIsNotRetried(t *testing.T) {
	m := NewManager(t.Context())
	if err := m.Apply(""); err != nil {
		t.Fatalf("applying an empty target failed: %v", err)
	}
	if _, retry := m.needsRetry(); retry {
		t.Error("a manager with no target wants a retry")
	}
	if m.Handle() != nil {
		t.Error("an empty target opened something")
	}

	// And it retires what was open: this is the off switch, not a no-op.
	_ = m.Apply("host='127.0.0.1' port='1' user='x' dbname='x' sslmode='disable'")
	if _, retry := m.needsRetry(); !retry {
		t.Fatal("precondition: the failed target should be pending a retry")
	}
	if err := m.Apply(""); err != nil {
		t.Fatalf("applying an empty target after a failure: %v", err)
	}
	if _, retry := m.needsRetry(); retry {
		t.Error("an emptied target is still retried, so recording cannot be switched off")
	}
}
