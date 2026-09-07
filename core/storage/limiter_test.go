package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockWindow is how long a test waits before concluding that something which
// must NOT happen has indeed not happened. Nothing is being synchronised on in
// those spots: the assertion is precisely that nothing further occurs.
const blockWindow = 200 * time.Millisecond

// newLimitedGate builds a gate with a shrunken bound and deadline. No Watch, so
// no probe loop can reach a verdict of its own and confuse a test. Never use
// the real 128 slots or the real 15s deadline in a test.
func newLimitedGate(capacity int, deadline time.Duration) *Gate {
	g := newGate(time.Hour, time.Hour, func(string) error { return nil })
	g.fsSem = make(chan struct{}, capacity)
	g.fsDeadline = deadline
	return g
}

// blockingInner stands in for a mount that has stopped answering: every call
// enters and stays there until release is closed. It records how many calls
// were inside it at the same time, which is what the bound is about.
type blockingInner struct {
	release chan struct{}
	entered chan struct{}
	opened  chan *recordingCloser

	live atomic.Int64
	peak atomic.Int64
}

func newBlockingInner() *blockingInner {
	return &blockingInner{
		release: make(chan struct{}),
		entered: make(chan struct{}, 256),
		opened:  make(chan *recordingCloser, 8),
	}
}

func (b *blockingInner) enter() error {
	n := b.live.Add(1)
	for {
		peak := b.peak.Load()
		if n <= peak || b.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	b.entered <- struct{}{}
	<-b.release
	b.live.Add(-1)
	return nil
}

func (b *blockingInner) ListFiles(context.Context, string) ([]FileInfo, error) {
	return nil, b.enter()
}

func (b *blockingInner) GetFile(context.Context, string) (io.ReadCloser, error) {
	rc := newRecordingCloser()
	select {
	case b.opened <- rc:
	default:
	}
	if err := b.enter(); err != nil {
		return nil, err
	}
	return rc, nil
}

func (b *blockingInner) DeletePath(context.Context, string) error { return b.enter() }
func (b *blockingInner) CreateDir(context.Context, string) error  { return b.enter() }
func (b *blockingInner) CopyToLocal(context.Context, string, string) error {
	return b.enter()
}

func (b *blockingInner) WriteFile(context.Context, string, io.Reader) error {
	return b.enter()
}

func (b *blockingInner) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", b.enter()
}

func (b *blockingInner) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// recordingCloser proves a ReadCloser was closed, which for the abandoned path
// is the difference between a returned fd and a leaked one.
type recordingCloser struct {
	io.Reader
	closes atomic.Int64
	closed chan struct{}
}

func newRecordingCloser() *recordingCloser {
	return &recordingCloser{Reader: strings.NewReader("payload"), closed: make(chan struct{}, 4)}
}

func (r *recordingCloser) Close() error {
	r.closes.Add(1)
	select {
	case r.closed <- struct{}{}:
	default:
	}
	return nil
}

// started runs fn on its own goroutine and returns a channel closed when it
// returns. Every wait in this file goes through it, so a lost slot shows up as
// a named failure rather than as the whole package timing out.
func started(fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

func mustFinish(t *testing.T, what string, fn func()) {
	t.Helper()
	select {
	case <-started(fn):
	case <-time.After(testDeadline):
		t.Fatalf("%s did not return within %v; a slot was most likely never released", what, testDeadline)
	}
}

func mustBlock(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s returned, want it still waiting for a free slot", what)
	case <-time.After(blockWindow):
	}
}

// TestGateReleasesSlotAfterCompletedCall pins the ordinary case: a slot is
// handed back, so a single-slot gate can serve call after call. Without it the
// first request would consume the pool for the life of the process.
func TestGateReleasesSlotAfterCompletedCall(t *testing.T) {
	const rounds = 5
	tests := []struct {
		name string
		run  func(StorageProvider) error
	}{
		{"ListFiles", func(p StorageProvider) error { _, err := p.ListFiles(context.Background(), "a"); return err }},
		{"DeletePath", func(p StorageProvider) error { return p.DeletePath(context.Background(), "a") }},
		{"CreateDir", func(p StorageProvider) error { return p.CreateDir(context.Background(), "a") }},
		{"DownloadURL", func(p StorageProvider) error {
			_, err := p.DownloadURL(context.Background(), "a", time.Minute)
			return err
		}},
		{"WriteFile", func(p StorageProvider) error {
			return p.WriteFile(context.Background(), "a", strings.NewReader("x"))
		}},
		{"CopyToLocal", func(p StorageProvider) error { return p.CopyToLocal(context.Background(), "a", "b") }},
		{"GetFile", func(p StorageProvider) error {
			rc, err := p.GetFile(context.Background(), "a")
			if err != nil {
				return err
			}
			return rc.Close()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewGatedProvider(&fakeInner{}, newLimitedGate(1, testDeadline))
			var err error
			mustFinish(t, tt.name+" run repeatedly through a single slot", func() {
				for i := 0; i < rounds; i++ {
					if err = tt.run(p); err != nil {
						return
					}
				}
			})
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
		})
	}
}

// TestGateDetachedDeadlineTripsGate covers the hang bound. The deadline is the
// only way out of a call that will never return, and the expiry carries no
// errno, so the gate has to be tripped by it directly.
func TestGateDetachedDeadlineTripsGate(t *testing.T) {
	inner := newBlockingInner()
	g := newLimitedGate(1, 20*time.Millisecond)
	p := NewGatedProvider(inner, g)

	var err error
	mustFinish(t, "an abandoned ListFiles", func() {
		_, err = p.ListFiles(context.Background(), "a")
	})
	if !errors.Is(err, ErrBackendUnreachable) {
		t.Fatalf("ListFiles error = %v, want it to match ErrBackendUnreachable", err)
	}
	if IsNotExist(err) {
		t.Errorf("abandoned call looks like a missing object: %v", err)
	}
	if ok, cause := g.Healthy(); ok {
		t.Errorf("Healthy() = (%v, %v) after a call was abandoned at its deadline, want the gate tripped", ok, cause)
	}

	// The abandoned call still holds the only slot, and that is the bound
	// working: the slot is taken for exactly as long as the kernel keeps the
	// syscall outstanding, not until the caller gave up.
	next := started(func() { _ = g.Do(context.Background(), func() error { return nil }) })
	mustBlock(t, "a call queued behind the abandoned one", next)

	close(inner.release)
	select {
	case <-next:
	case <-time.After(testDeadline):
		t.Fatal("the slot was never returned after the abandoned call finally came back")
	}
}

// TestGateBoundsConcurrency is the crash fix. Thread consumption on this path
// must be a constant, not a function of traffic, so no number of concurrent
// requests may put more than the cap inside a syscall at once.
func TestGateBoundsConcurrency(t *testing.T) {
	const (
		capacity = 3
		callers  = 24
	)
	inner := newBlockingInner()
	p := NewGatedProvider(inner, newLimitedGate(capacity, testDeadline))

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.ListFiles(context.Background(), "a")
		}()
	}
	for i := 0; i < capacity; i++ {
		recv(t, inner.entered, "a call to enter the filesystem")
	}

	// The cap is now taken and every other caller must be parked on the
	// semaphore, which costs heap rather than an OS thread. Nothing to
	// synchronise on: the assertion is that no further call enters.
	time.Sleep(blockWindow)
	if n := inner.live.Load(); n > capacity {
		t.Errorf("%d calls inside the filesystem at once, want at most %d", n, capacity)
	}

	close(inner.release)
	mustFinish(t, "all callers", wg.Wait)
	if n := inner.peak.Load(); n > capacity {
		t.Fatalf("peak concurrency was %d with a cap of %d; %d concurrent requests must never put more than the cap inside a syscall", n, capacity, callers)
	}
}

// TestGetFileHoldsItsSlotUntilClose: the streaming reads happen in the handler,
// outside this package, so releasing at return would leave a mount that wedges
// mid-download blocking a goroutine nothing here bounds.
func TestGetFileHoldsItsSlotUntilClose(t *testing.T) {
	p := NewGatedProvider(&fakeInner{body: "payload"}, newLimitedGate(1, testDeadline))

	rc, err := p.GetFile(context.Background(), "a")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}

	second := started(func() {
		rc2, err := p.GetFile(context.Background(), "b")
		if err == nil {
			_ = rc2.Close()
		}
	})
	mustBlock(t, "a second GetFile while the first ReadCloser is open", second)

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-second:
	case <-time.After(testDeadline):
		t.Fatal("the second GetFile never proceeded after the first ReadCloser was closed")
	}
}

// TestGetFileDoubleCloseReleasesOnce: a defer plus an explicit close on the
// happy path is a real pattern here. Releasing twice would hand out a slot that
// does not exist and the bound would be gone.
func TestGetFileDoubleCloseReleasesOnce(t *testing.T) {
	p := NewGatedProvider(&fakeInner{body: "payload"}, newLimitedGate(1, testDeadline))

	rc, err := p.GetFile(context.Background(), "a")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// A second release has nowhere to go and blocks here rather than corrupting
	// the count, so a Close that does not return is the symptom.
	mustFinish(t, "a second Close", func() { _ = rc.Close() })

	// Exactly one slot came back, not two.
	held, err := p.GetFile(context.Background(), "b")
	if err != nil {
		t.Fatalf("GetFile after the double Close: %v", err)
	}
	defer held.Close()
	third := started(func() {
		rc3, err := p.GetFile(context.Background(), "c")
		if err == nil {
			_ = rc3.Close()
		}
	})
	mustBlock(t, "a third GetFile after a double Close", third)
}

// TestAbandonedGetFileClosesLateReadCloser: an open that lands after everyone
// gave up on it still opened an fd. Nobody else can reach it, so the abandoned
// path has to close it.
func TestAbandonedGetFileClosesLateReadCloser(t *testing.T) {
	inner := newBlockingInner()
	p := NewGatedProvider(inner, newLimitedGate(1, 20*time.Millisecond))

	var rc io.ReadCloser
	var err error
	mustFinish(t, "an abandoned GetFile", func() {
		rc, err = p.GetFile(context.Background(), "a")
	})
	if !errors.Is(err, ErrBackendUnreachable) {
		t.Fatalf("GetFile error = %v, want it to match ErrBackendUnreachable", err)
	}
	if rc != nil {
		t.Fatalf("GetFile returned a %T alongside its error, want nil", rc)
	}

	late := recv(t, inner.opened, "the ReadCloser the inner provider opened")
	close(inner.release)
	recv(t, late.closed, "the late ReadCloser to be closed")
	if n := late.closes.Load(); n != 1 {
		t.Fatalf("late ReadCloser closed %d times, want exactly 1", n)
	}
}

// TestRunIsBoundedButHasNoDeadline: WriteFile and CopyToLocal cannot be
// abandoned (they read a caller-owned body that net/http invalidates once the
// handler returns), and a deadline cannot tell a slow multi-GB upload from a
// wedge. So they queue, and then they wait as long as it takes.
func TestRunIsBoundedButHasNoDeadline(t *testing.T) {
	const capacity = 1
	inner := newBlockingInner()
	g := newLimitedGate(capacity, 20*time.Millisecond)
	p := NewGatedProvider(inner, g)

	var err error
	slow := started(func() {
		err = p.WriteFile(context.Background(), "a", strings.NewReader("x"))
	})
	recv(t, inner.entered, "WriteFile to enter the filesystem")

	queued := started(func() {
		_ = p.WriteFile(context.Background(), "b", strings.NewReader("y"))
	})
	mustBlock(t, "a second WriteFile", queued)

	// Many deadline periods have passed by now. The call must still be running.
	select {
	case <-slow:
		t.Fatal("WriteFile returned while the inner call was still running; the inline path must have no deadline")
	default:
	}

	close(inner.release)
	select {
	case <-slow:
	case <-time.After(testDeadline):
		t.Fatal("WriteFile never returned")
	}
	if err != nil {
		t.Errorf("WriteFile = %v, want the slow call to complete rather than be abandoned", err)
	}
	if ok, cause := g.Healthy(); !ok {
		t.Errorf("Healthy() = (false, %v) after a slow inline write, want it untouched: a slow upload is not evidence about the mount", cause)
	}
	<-queued
}

// TestGateLimiterNilSafe: an AppState built without a gate (the connection test
// and the migration target pass nil on purpose) must take the same code path,
// unbounded but working, not a second one.
func TestGateLimiterNilSafe(t *testing.T) {
	var g *Gate
	mustFinish(t, "Do on a nil gate", func() {
		if err := g.Do(context.Background(), func() error { return nil }); err != nil {
			t.Errorf("Do on a nil gate = %v, want nil", err)
		}
	})
	mustFinish(t, "Run on a nil gate", func() {
		if err := g.Run(context.Background(), func() error { return nil }); err != nil {
			t.Errorf("Run on a nil gate = %v, want nil", err)
		}
	})

	p := NewGatedProvider(&fakeInner{body: "payload"}, nil)
	rc, err := p.GetFile(context.Background(), "a")
	if err != nil {
		t.Fatalf("GetFile through a nil gate: %v", err)
	}
	body, _ := io.ReadAll(rc)
	if err := rc.Close(); err != nil {
		t.Fatalf("Close through a nil gate: %v", err)
	}
	if string(body) != "payload" {
		t.Errorf("GetFile body = %q, want %q", body, "payload")
	}
}

// A panic inside a detached call must surface on the CALLER's goroutine.
// Detaching moved the call off the request goroutine, where net/http recovers a
// panic into one failed request; leaving it on the worker would turn the same
// bug into a process-wide crash. The slot must come back either way, or one
// panicking call would permanently shrink the pool.
func TestDetachedPanicResurfacesOnCallerAndReleasesSlot(t *testing.T) {
	g := newLimitedGate(1, time.Hour)

	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Fatalf("recover() = %v, want the original panic value %q", p, "boom")
			}
		}()
		_ = g.Do(context.Background(), func() error { panic("boom") })
		t.Fatal("Do returned normally, want the panic to propagate to the caller")
	}()

	// The only slot must be free again, so an ordinary call goes straight
	// through rather than queueing behind the panicking one.
	done := make(chan error, 1)
	go func() { done <- g.Do(context.Background(), func() error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Do after a panic = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Do after a panic blocked, so the panicking call leaked its slot")
	}
}

// A panic in a call the caller already ABANDONED has nowhere to be handed back
// to. It must not take the process down, and it must still return the slot.
func TestAbandonedPanicDoesNotCrashAndReleasesSlot(t *testing.T) {
	g := newLimitedGate(1, 20*time.Millisecond)

	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- g.Do(context.Background(), func() error {
			<-release
			panic("late boom")
		})
	}()

	// The caller gives up on the deadline while the call is still inside fn.
	if err := <-first; !errors.Is(err, ErrBackendUnreachable) {
		t.Fatalf("abandoned Do = %v, want an ErrBackendUnreachable match", err)
	}

	// Now let the abandoned worker finish and panic with nobody waiting.
	close(release)

	done := make(chan error, 1)
	go func() { done <- g.Do(context.Background(), func() error { return nil }) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the abandoned panicking call never released its slot")
	}
}

// TestSaturatedGateHonoursAcquireDeadline pins the property the per-request
// provider build relies on: when every slot is held, the wait for one is bounded
// by the CALLER's context, not left to run forever. A build that passed
// context.Background() here would block until a slot freed, which on a mount
// held open by in-flight downloads is unbounded and undrainable at shutdown.
func TestSaturatedGateHonoursAcquireDeadline(t *testing.T) {
	g := newLimitedGate(1, testDeadline)

	// Hold the single slot.
	inner := newBlockingInner()
	p := NewGatedProvider(inner, g)
	held := started(func() { _, _ = p.ListFiles(context.Background(), "a") })
	recv(t, inner.entered, "the holder to take the slot")
	t.Cleanup(func() { close(inner.release); <-held })

	// A second acquire with a short deadline must give up, not hang.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- g.Do(ctx, func() error { return nil }) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Do on a saturated gate = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(testDeadline):
		t.Fatal("Do on a saturated gate never returned; the acquire ignored its context deadline")
	}
}
