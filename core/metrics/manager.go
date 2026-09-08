package metrics

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// Manager owns the recorder that is currently in use and can replace it while
// Core runs.
//
// Without it the metrics target is whatever boot found, and an admin who points
// the panel at a different database is told to restart the platform to save a
// statistics setting. The swap is small because Recorder.Run already flushes
// once when its context ends: retiring one is cancel, wait, close.
//
// Everything here tolerates being empty. A Manager with no handle is the normal
// state on a fresh install (the feature is off) and after an unreachable
// database (Recorder.Observe ignores a nil receiver), so no caller has to check
// before recording.
type Manager struct {
	// base outlives every handle; each one gets a child context that is
	// cancelled when that handle is retired.
	base context.Context

	mu   sync.RWMutex
	cur  *Handle
	dsn  string
	stop context.CancelFunc

	// want is the target an operator has CONFIGURED, whether or not it is
	// currently open, and applied says Apply has been called at least once.
	//
	// The pair exists because opening used to be a single attempt at boot. A
	// Core that started 51 seconds before its metrics database was resolvable
	// logged one line and recorded nothing for the life of the process - and
	// since sampling is leader-gated, that Core happening to be the leader
	// stopped recording for the whole cluster while the other replica sat on a
	// working connection doing nothing. Measured in production on 2026-09-03,
	// during an ordinary stack deploy.
	want    string
	applied bool
	// failing keeps the retry loop from logging once per attempt: one line when
	// it breaks, one when it recovers.
	failing bool
	// done closes when the retired recorder's Run has returned, which is also
	// when its final flush has finished. Waiting on it is what stops a swap from
	// closing the database out from under a flush in progress.
	done chan struct{}
}

// NewManager returns a Manager with nothing open yet. Call Apply to open one.
func NewManager(base context.Context) *Manager {
	m := &Manager{base: base}
	go m.Watch(base)
	return m
}

// Handle is the current handle, or nil. Readers must cope with nil - see the
// note on AppState.Metrics.
func (m *Manager) Handle() *Handle {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur
}

// Recorder is the current recorder, or nil. The collector reads through this on
// every sample rather than holding one, so a swap takes effect immediately
// instead of at the next restart.
func (m *Manager) Recorder() *Recorder {
	h := m.Handle()
	if h == nil {
		return nil
	}
	return h.Recorder
}

// DSN reports the target currently in use: empty means the Core database.
// Only for reporting - the settings layer owns what it SHOULD be.
func (m *Manager) DSN() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dsn
}

// Apply switches to dsn, opening it first and keeping the old one on failure.
//
// The order is the whole point: a bad target reported by the panel must not also
// have stopped the recording that was working. Nothing is retired until the
// replacement is open and its schema exists.
//
// An unchanged dsn is a no-op, so a settings save that touched something else
// does not interrupt recording for a few seconds and lose the buckets in flight.
func (m *Manager) Apply(dsn string) error {
	// Nil-tolerant like every other method here, and it reports rather than
	// pretends: a caller that gets no error is entitled to believe the target
	// is in use. The settings handler turns this into "saved, not yet
	// recording" instead of a panic on a screen nobody expects to be fatal.
	if m == nil {
		return errors.New("no metrics manager is running")
	}

	// No target is not an outage: it is the configured state of an installation
	// that records nothing, and it is what switching recording off looks like
	// from here. Close rather than Open, so the retry loop does not spend the
	// rest of the process trying to reopen a database nobody named.
	if dsn == "" {
		m.Close()
		return nil
	}

	m.mu.Lock()
	m.want, m.applied = dsn, true
	same := m.cur != nil && m.dsn == dsn
	m.mu.Unlock()
	if same {
		return nil
	}

	ctx, cancel := context.WithCancel(m.base)
	h, err := Open(ctx, dsn)
	if err != nil {
		cancel()
		return err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Recorder.Run(ctx, FlushInterval)
	}()

	m.mu.Lock()
	old, oldStop, oldDone := m.cur, m.stop, m.done
	m.cur, m.dsn, m.stop, m.done = h, dsn, cancel, done
	recovered := m.failing
	m.failing = false
	m.mu.Unlock()

	if recovered {
		log.Println("metrics: the statistics database is reachable again; recording resumed")
	}
	retire(old, oldStop, oldDone)
	return nil
}

// retryInterval is how often an unopened target is tried again.
//
// Short, because every tick that passes without a recorder is a sample the
// collector throws away - Observe on a nil recorder is a no-op by design, so
// nothing queues up waiting for the database to come back.
const retryInterval = 15 * time.Second

// Watch reopens a configured target that is not open, until ctx ends.
//
// Started by NewManager rather than by its caller, deliberately: the failure it
// exists to prevent was a single attempt with nobody to make a second one, and
// a retry that has to be wired up separately is a retry somebody can forget to
// wire up.
func (m *Manager) Watch(ctx context.Context) {
	if m == nil {
		return
	}
	t := time.NewTicker(retryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			dsn, retry := m.needsRetry()
			if !retry {
				continue
			}
			if err := m.Apply(dsn); err != nil {
				m.mu.Lock()
				first := !m.failing
				m.failing = true
				m.mu.Unlock()
				if first {
					log.Printf("metrics: the statistics database is unreachable, retrying every %s (%v)", retryInterval, err)
				}
			}
		}
	}
}

// needsRetry reports the target to reopen, and whether there is one.
//
// Nothing to do before the first Apply (boot has not chosen a target yet) or
// after Close (shutdown, or an operator switching recording off) - reopening in
// either case would be this loop deciding policy it does not own.
func (m *Manager) needsRetry() (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.want, m.applied && m.cur == nil
}

// Close retires whatever is open. Safe on a Manager that never opened one.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	old, oldStop, oldDone := m.cur, m.stop, m.done
	m.cur, m.dsn, m.stop, m.done = nil, "", nil, nil
	// applied goes back to false so the retry loop does not treat a deliberate
	// close as an outage and reopen what was just shut down.
	m.want, m.applied, m.failing = "", false, false
	m.mu.Unlock()
	retire(old, oldStop, oldDone)
}

// retireTimeout bounds the wait for a retired recorder's final flush. Longer
// than the flush's own 5s budget so the normal case completes, short enough that
// a wedged database cannot hold a settings save open.
const retireTimeout = 10 * time.Second

// retire stops a recorder, waits for its last flush, then closes its database.
//
// Closing before that wait is the bug this exists to avoid: Run flushes once on
// cancellation, and a database closed underneath it turns the final minutes into
// a "store write failed" line instead of rows.
func retire(h *Handle, stop context.CancelFunc, done chan struct{}) {
	if h == nil {
		return
	}
	if stop != nil {
		stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(retireTimeout):
			log.Printf("metrics: the previous recorder did not finish its final flush within %s; closing anyway", retireTimeout)
		}
	}
	if err := h.Close(); err != nil {
		log.Printf("metrics: closing the previous metrics database: %v", err)
	}
}
