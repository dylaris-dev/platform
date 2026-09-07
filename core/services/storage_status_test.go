package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"dylaris-core/storage"

	"github.com/redis/go-redis/v9"
)

// sentinelCause is the distinctive text every fault in this file carries. It
// stands in for the things a real cause leaks: the host path, the bucket, the
// endpoint host. No published payload may contain it.
const sentinelCause = "secret-bucket-hostname"

// gateFault is a cause the gate's classifier accepts. IsBackendUnreachable only
// answers for errors carrying one of its errnos, so a bare sentinel string
// would be ignored and the gate would never trip.
func gateFault() error {
	return fmt.Errorf("%s: %w", sentinelCause, syscall.EIO)
}

// s3ConnFault is a cause the AWS SDK's retry taxonomy classifies as a
// connection failure, which is what S3Resilience reacts to. Anything it calls a
// refusal (a 403, a missing key) deliberately does not change the state.
type s3ConnFault struct{}

func (s3ConnFault) Error() string         { return sentinelCause + ": connection reset by peer" }
func (s3ConnFault) ConnectionError() bool { return true }
func (s3ConnFault) Temporary() bool       { return true }

// failingS3Inner is the provider underneath the resilient wrapper. Only
// WriteFile is exercised: it is the one operation that reports a fault without
// entering the retry loop, so a test drives a real transition without waiting
// out a retry interval.
type failingS3Inner struct {
	err error
}

func (f *failingS3Inner) ListFiles(context.Context, string) ([]storage.FileInfo, error) {
	return nil, f.err
}
func (f *failingS3Inner) GetFile(context.Context, string) (io.ReadCloser, error) { return nil, f.err }
func (f *failingS3Inner) DeletePath(context.Context, string) error               { return f.err }
func (f *failingS3Inner) CreateDir(context.Context, string) error                { return f.err }
func (f *failingS3Inner) CopyToLocal(context.Context, string, string) error      { return f.err }
func (f *failingS3Inner) WriteFile(context.Context, string, io.Reader) error     { return f.err }
func (f *failingS3Inner) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", f.err
}

func (f *failingS3Inner) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// storageEvent is the published payload, decoded.
type storageEvent struct {
	Backend string  `json:"backend"`
	State   string  `json:"state"`
	Since   *string `json:"since"`
}

// newStatusFixture wires a StorageStatus onto a miniredis-backed publisher with
// its hooks attached, and returns a drained subscriber channel. The forwarder is
// NOT started: tests call flush themselves so no assertion depends on goroutine
// timing.
func newStatusFixture(t *testing.T) (*StorageStatus, *storage.Gate, *storage.S3Resilience, <-chan *redis.Message) {
	t.Helper()
	rdb := newQueueTestRedis(t)
	gate := storage.NewGate()
	s3 := storage.NewS3Resilience()
	s := NewStorageStatus(NewSystemEventsPublisher(rdb), gate, s3)
	s.Attach()

	ctx := context.Background()
	sub := rdb.Subscribe(ctx, SystemEventsChannel)
	t.Cleanup(func() { sub.Close() })
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return s, gate, s3, sub.Channel()
}

// recvEvent takes the next published event, failing if none arrives.
func recvEvent(t *testing.T, ch <-chan *redis.Message) (SystemEvent, storageEvent, string) {
	t.Helper()
	select {
	case msg := <-ch:
		var ev SystemEvent
		if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			t.Fatalf("re-marshal payload: %v", err)
		}
		var se storageEvent
		if err := json.Unmarshal(raw, &se); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		return ev, se, msg.Payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a published event")
	}
	return SystemEvent{}, storageEvent{}, ""
}

// expectNoEvent asserts the channel stays quiet, which is how "exactly one
// event" is pinned.
func expectNoEvent(t *testing.T, ch <-chan *redis.Message) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra event: %s", msg.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStorageStatus_GateTransitionsPublish drives the REAL gate, so it proves
// Attach installed the hook on the right backend as well as the mapping.
func TestStorageStatus_GateTransitionsPublish(t *testing.T) {
	tests := []struct {
		name      string
		drive     func(g *storage.Gate)
		wantState string
		wantSince bool
	}{
		{
			name:      "unreachable evidence trips to unavailable",
			drive:     func(g *storage.Gate) { g.ReportFailure(gateFault()) },
			wantState: storageStateUnavailable,
			wantSince: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, gate, _, ch := newStatusFixture(t)
			tc.drive(gate)
			s.flush(context.Background())

			ev, se, _ := recvEvent(t, ch)
			if ev.Type != StorageConnectionChangedEvent {
				t.Errorf("Type = %q, want %q", ev.Type, StorageConnectionChangedEvent)
			}
			if se.Backend != storageBackendPath {
				t.Errorf("backend = %q, want %q", se.Backend, storageBackendPath)
			}
			if se.State != tc.wantState {
				t.Errorf("state = %q, want %q", se.State, tc.wantState)
			}
			if tc.wantSince && (se.Since == nil || *se.Since == "") {
				t.Errorf("since = %v, want a timestamp", se.Since)
			}
			expectNoEvent(t, ch)
		})
	}
}

// TestStorageStatus_GateRecoveryPublishesOK pins the other half of the gate's
// polarity: healthy=true must become ok, with no since.
func TestStorageStatus_GateRecoveryPublishesOK(t *testing.T) {
	s, gate, _, ch := newStatusFixture(t)

	gate.ReportFailure(gateFault())
	s.flush(context.Background())
	if _, se, _ := recvEvent(t, ch); se.State != storageStateUnavailable {
		t.Fatalf("setup state = %q, want %q", se.State, storageStateUnavailable)
	}

	// Stop returns the gate to healthy, which is a transition and fires the hook.
	gate.Stop()
	s.flush(context.Background())

	_, se, _ := recvEvent(t, ch)
	if se.Backend != storageBackendPath {
		t.Errorf("backend = %q, want %q", se.Backend, storageBackendPath)
	}
	if se.State != storageStateOK {
		t.Errorf("state = %q, want %q", se.State, storageStateOK)
	}
	if se.Since != nil {
		t.Errorf("since = %v, want null for an ok state", *se.Since)
	}
}

// TestStorageStatus_S3PolarityIsNormalised is the polarity guard. The gate
// reports HEALTHY and S3 reports RECONNECTING, opposite senses of the same
// boolean, so a swapped mapping would publish "reconnecting" on recovery and
// "ok" during an outage. Both directions are asserted, driven through the real
// resilient provider so the wiring is proven too.
func TestStorageStatus_S3PolarityIsNormalised(t *testing.T) {
	s, _, s3, ch := newStatusFixture(t)
	ctx := context.Background()

	inner := &failingS3Inner{err: s3ConnFault{}}
	prov := storage.NewS3ResilientProvider(inner, s3)

	if err := prov.WriteFile(ctx, "x", strings.NewReader("data")); err == nil {
		t.Fatal("WriteFile: want the connection fault back, got nil")
	}
	if reconnecting, _, _ := s3.State(); !reconnecting {
		t.Fatal("precondition: the backend did not enter reconnecting")
	}
	s.flush(ctx)

	_, se, _ := recvEvent(t, ch)
	if se.Backend != storageBackendS3 {
		t.Errorf("backend = %q, want %q", se.Backend, storageBackendS3)
	}
	if se.State != storageStateReconnecting {
		t.Errorf("state = %q, want %q (reconnecting=true must NOT map to ok)", se.State, storageStateReconnecting)
	}
	if se.Since == nil || *se.Since == "" {
		t.Errorf("since = %v, want a timestamp", se.Since)
	}

	// Recovery is driven with a RETRIED operation, not another WriteFile. The
	// write path waits for the reconnecting state to clear before it starts, and
	// nothing here would ever clear it, so it would sit out the full retry budget.
	inner.err = nil
	if _, err := prov.ListFiles(ctx, "x"); err != nil {
		t.Fatalf("ListFiles after recovery: %v", err)
	}
	s.flush(ctx)

	_, se, _ = recvEvent(t, ch)
	if se.State != storageStateOK {
		t.Errorf("state = %q, want %q (reconnecting=false must NOT map to reconnecting)", se.State, storageStateOK)
	}
	if se.Since != nil {
		t.Errorf("since = %v, want null for an ok state", *se.Since)
	}
}

// TestStorageStatus_PayloadCarriesNoCause is the security guard. /api/system
// /events is behind AuthMiddleware with no per-event filtering, so every
// authenticated user reads whatever goes into this payload. The cause text
// names the path, the bucket and the endpoint, and must never appear.
func TestStorageStatus_PayloadCarriesNoCause(t *testing.T) {
	tests := []struct {
		name  string
		drive func(t *testing.T, g *storage.Gate, r *storage.S3Resilience)
	}{
		{
			name:  "gate cause",
			drive: func(_ *testing.T, g *storage.Gate, _ *storage.S3Resilience) { g.ReportFailure(gateFault()) },
		},
		{
			name: "s3 lastErr",
			drive: func(t *testing.T, _ *storage.Gate, r *storage.S3Resilience) {
				prov := storage.NewS3ResilientProvider(&failingS3Inner{err: s3ConnFault{}}, r)
				if err := prov.WriteFile(context.Background(), "x", strings.NewReader("d")); err == nil {
					t.Fatal("want the connection fault back")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, gate, s3, ch := newStatusFixture(t)
			tc.drive(t, gate, s3)
			s.flush(context.Background())

			_, _, raw := recvEvent(t, ch)
			if strings.Contains(raw, sentinelCause) {
				t.Errorf("published payload leaks the cause text %q: %s", sentinelCause, raw)
			}
		})
	}
}

// TestStorageStatus_CoalescesRapidTransitions pins the three properties the
// single-slot design exists for: a hook never queues per-transition work, the
// backends do not evict each other, and the LAST state is the one that reaches
// the wire. The forwarder is intentionally not running, so N transitions land
// while nothing drains.
//
// What this also documents is the cost: the intermediate states below are
// dropped and never published at all.
func TestStorageStatus_CoalescesRapidTransitions(t *testing.T) {
	s, _, _, ch := newStatusFixture(t)

	const n = 50
	for i := 0; i < n; i++ {
		s.onGateChange(i%2 == 0, gateFault())
		s.onS3Change(i%2 == 0, time.Unix(1700000000, 0).UTC(), s3ConnFault{})
	}
	// The last write above is healthy=false / reconnecting=false.

	if got := len(s.wake); got != 1 {
		t.Errorf("wake queue = %d after %d transitions, want 1 (capacity-1 coalescing)", got, 2*n)
	}
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending != 2 {
		t.Errorf("pending = %d, want 2 (one per backend, latest state only)", pending)
	}

	s.flush(context.Background())

	_, path, _ := recvEvent(t, ch)
	if path.Backend != storageBackendPath || path.State != storageStateUnavailable {
		t.Errorf("path event = %+v, want backend=path state=%s", path, storageStateUnavailable)
	}
	_, s3ev, _ := recvEvent(t, ch)
	if s3ev.Backend != storageBackendS3 || s3ev.State != storageStateOK {
		t.Errorf("s3 event = %+v, want backend=s3 state=%s", s3ev, storageStateOK)
	}
	expectNoEvent(t, ch)
}

// TestStorageStatus_StartForwards covers the run loop itself, which the flush
// -driven tests above deliberately bypass.
func TestStorageStatus_StartForwards(t *testing.T) {
	s, gate, _, ch := newStatusFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	gate.ReportFailure(gateFault())

	_, se, _ := recvEvent(t, ch)
	if se.Backend != storageBackendPath || se.State != storageStateUnavailable {
		t.Errorf("event = %+v, want backend=path state=%s", se, storageStateUnavailable)
	}
}

// TestStorageStatus_SnapshotReadsBackendsLive is the guard on the no-mirror
// rule: the gate is tripped with NO hook attached, so nothing this type
// observed changed. A cached copy of the boolean would still say ok here.
func TestStorageStatus_SnapshotReadsBackendsLive(t *testing.T) {
	gate := storage.NewGate()
	s3 := storage.NewS3Resilience()
	s := NewStorageStatus(nil, gate, s3)
	// Attach is deliberately NOT called.

	if snap := s.Snapshot(); snap.Path.State != storageStateOK {
		t.Fatalf("precondition: path = %q, want %q", snap.Path.State, storageStateOK)
	}

	gate.ReportFailure(gateFault())

	snap := s.Snapshot()
	if snap.Path.State != storageStateUnavailable {
		t.Errorf("path = %q, want %q (Snapshot must read the gate, not a mirror)", snap.Path.State, storageStateUnavailable)
	}
	if snap.Path.Since != nil {
		t.Errorf("since = %v, want null: no hook fired, so the transition time was never observed", *snap.Path.Since)
	}
	if snap.S3.State != storageStateOK {
		t.Errorf("s3 = %q, want %q", snap.S3.State, storageStateOK)
	}
}

// TestStorageStatus_SnapshotSinceIsPinnable proves the injected clock, and that
// an observed transition does carry its timestamp.
func TestStorageStatus_SnapshotSinceIsPinnable(t *testing.T) {
	fixed := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	gate := storage.NewGate()
	s := NewStorageStatus(nil, gate, nil)
	s.now = func() time.Time { return fixed }
	s.Attach()

	gate.ReportFailure(gateFault())

	snap := s.Snapshot()
	if snap.Path.Since == nil {
		t.Fatal("since = nil, want the pinned timestamp")
	}
	if want := "2026-07-20T10:00:00Z"; *snap.Path.Since != want {
		t.Errorf("since = %q, want %q", *snap.Path.Since, want)
	}
}

// TestStorageStatus_NilSafety pins the contract that lets callers use this
// unconditionally, matching SystemEventsPublisher.Publish and every method on
// Gate and S3Resilience.
func TestStorageStatus_NilSafety(t *testing.T) {
	ctx := context.Background()

	t.Run("nil receiver", func(t *testing.T) {
		var s *StorageStatus
		s.Start(ctx)
		s.Attach()
		snap := s.Snapshot()
		if snap.Path.State != storageStateOK || snap.S3.State != storageStateOK {
			t.Errorf("snapshot = %+v, want both ok", snap)
		}
	})

	t.Run("nil backends and nil publisher", func(t *testing.T) {
		s := NewStorageStatus(nil, nil, nil)
		s.Attach()
		s.onGateChange(false, gateFault())
		s.onS3Change(true, time.Now(), s3ConnFault{})
		s.flush(ctx) // must not panic on a nil publisher
		if snap := s.Snapshot(); snap.Path.State != storageStateOK || snap.S3.State != storageStateOK {
			t.Errorf("snapshot = %+v, want both ok from nil backends", snap)
		}
	})
}
