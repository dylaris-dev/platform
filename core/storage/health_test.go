package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// testDeadline bounds every wait in this file. It is generous on purpose: the
// tests never rely on it elapsing, only on it catching a hang, so a slow or
// race-instrumented CI machine cannot make them flaky.
const testDeadline = 10 * time.Second

// hangingProbe blocks until release is closed, which is how these tests stand
// in for a stat on a mount that has stopped answering. starts counts entries so
// a test can assert how many probes were ever begun.
func hangingProbe(release <-chan struct{}, starts *atomic.Int64, entered chan<- struct{}) func(string) error {
	return func(string) error {
		starts.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testDeadline):
		var zero T
		t.Fatalf("timed out waiting for %s", what)
		return zero
	}
}

func TestGateStartsHealthy(t *testing.T) {
	tests := []struct {
		name string
		gate *Gate
	}{
		{"production constructor", NewGate()},
		// Nil-safe by design: an AppState built without a gate must take the
		// same code path as the wired-up one, not a second one.
		{"nil gate", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, cause := tt.gate.Healthy()
			if !ok || cause != nil {
				t.Errorf("Healthy() = (%v, %v), want (true, nil): a gate that starts unhealthy would refuse every request on a fresh boot", ok, cause)
			}
		})
	}
}

func TestGateHangingProbeTripsUnhealthy(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	var starts atomic.Int64
	entered := make(chan struct{}, 1)
	g := newGate(time.Millisecond, 5*time.Millisecond, hangingProbe(release, &starts, entered))

	changes := make(chan bool, 4)
	g.SetOnChange(func(healthy bool, _ error) { changes <- healthy })
	g.Watch("/mnt/wedged")
	defer g.Stop()

	recv(t, entered, "the first probe to start")
	if healthy := recv(t, changes, "the gate to trip"); healthy {
		t.Fatal("first transition = healthy, want unhealthy after a probe that never returns")
	}
	ok, cause := g.Healthy()
	if ok {
		t.Error("Healthy() = true after the probe timed out")
	}
	if cause == nil {
		t.Error("Healthy() reported unhealthy with a nil cause; an unhealthy gate must always say why")
	}
}

// TestGateProbeDoesNotPileUp is the leak bound. The capacity-1 semaphore is
// what keeps a wedged mount to ONE abandoned goroutine in total rather than one
// per tick, and the only way to see it is to count probe starts.
func TestGateProbeDoesNotPileUp(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	var starts atomic.Int64
	entered := make(chan struct{}, 1)
	g := newGate(time.Millisecond, 5*time.Millisecond, hangingProbe(release, &starts, entered))
	g.Watch("/mnt/wedged")
	defer g.Stop()

	recv(t, entered, "the first probe to start")

	// Hundreds of ticks are due in this window. Every one of them must find
	// the semaphore taken and skip. There is nothing to synchronise on here:
	// the assertion is precisely that nothing further happens.
	time.Sleep(200 * time.Millisecond)

	if n := starts.Load(); n != 1 {
		t.Fatalf("probe started %d times while one was still outstanding, want exactly 1; each extra start is another goroutine abandoned for the life of the outage", n)
	}
}

func TestGateRecoversWhenProbeSucceedsAgain(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	g := newGate(time.Millisecond, testDeadline, func(string) error {
		if failing.Load() {
			return &fs.PathError{Op: "stat", Path: "/mnt/nas", Err: syscall.EIO}
		}
		return nil
	})

	changes := make(chan bool, 8)
	g.SetOnChange(func(healthy bool, _ error) { changes <- healthy })
	g.Watch("/mnt/nas")
	defer g.Stop()

	if healthy := recv(t, changes, "the gate to trip"); healthy {
		t.Fatal("first transition = healthy, want unhealthy")
	}
	failing.Store(false)
	if healthy := recv(t, changes, "the gate to recover"); !healthy {
		t.Fatal("second transition = unhealthy, want a return to healthy once the probe succeeds again")
	}
	if ok, cause := g.Healthy(); !ok || cause != nil {
		t.Errorf("Healthy() = (%v, %v) after recovery, want (true, nil)", ok, cause)
	}
}

// TestGateProbeErrorClassification pins which probe RESULTS are treated as an
// outage. A probe that returned at all means the filesystem answered, so only
// the classifier's verdict trips the gate. ENOENT in particular must not: on a
// fresh install nothing has created the root yet.
func TestGateProbeErrorClassification(t *testing.T) {
	tests := []struct {
		name        string
		probeErr    error
		wantHealthy bool
	}{
		{"stat succeeds", nil, true},
		{"root does not exist yet", &fs.PathError{Op: "stat", Path: "/mnt/nas", Err: syscall.ENOENT}, true},
		{"permission problem is config, not outage", &fs.PathError{Op: "stat", Path: "/mnt/nas", Err: syscall.EACCES}, true},
		{"transport is down", &fs.PathError{Op: "stat", Path: "/mnt/nas", Err: syscall.EHOSTDOWN}, false},
		{"stale nfs handle", &fs.PathError{Op: "stat", Path: "/mnt/nas", Err: syscall.ESTALE}, false},
	}
	// Each case runs from both start states, because the probe verdict is what
	// decides the answer either way: a failing probe must trip a healthy gate,
	// and a probe that proves the filesystem answers must clear a tripped one.
	starts := []struct {
		name string
		trip bool
	}{
		{"from healthy", false},
		{"from unhealthy", true},
	}
	for _, tt := range tests {
		for _, start := range starts {
			t.Run(tt.name+"/"+start.name, func(t *testing.T) {
				probed := make(chan struct{}, 8)
				g := newGate(time.Millisecond, testDeadline, func(string) error {
					select {
					case probed <- struct{}{}:
					default:
					}
					return tt.probeErr
				})
				g.Watch("/mnt/nas")
				defer g.Stop()
				if start.trip {
					g.ReportFailure(syscall.EIO)
				}

				// A probe can only START once the previous one's verdict has
				// been recorded (the semaphore and the loop see to that), so
				// waiting for further starts is what makes this deterministic.
				// Waiting on the probe start alone would read the state before
				// that probe's own verdict existed.
				drain(probed)
				for i := 0; i < 4; i++ {
					recv(t, probed, "a further probe to start")
				}

				if ok, _ := g.Healthy(); ok != tt.wantHealthy {
					t.Errorf("Healthy() = %v after a probe returning %v, want %v", ok, tt.probeErr, tt.wantHealthy)
				}
			})
		}
	}
}

func drain[T any](ch <-chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// TestGateReportFailure covers the immediate trip from the request path. The
// errno filtering is the whole guard: without it a single missing file or one
// chmod'ed key would take the entire backend offline.
func TestGateReportFailure(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantHealthy bool
	}{
		{"unreachable errno trips immediately", &fs.PathError{Op: "open", Path: "/mnt/nas/a", Err: syscall.EIO}, false},
		{"bare unreachable errno", syscall.ETIMEDOUT, false},
		{"rename across a dead mount", &os.LinkError{Op: "rename", Err: syscall.ECONNRESET}, false},

		{"missing object is routine", &fs.PathError{Op: "open", Path: "/mnt/nas/a", Err: syscall.ENOENT}, true},
		{"permission problem is config, not outage", &fs.PathError{Op: "open", Path: "/mnt/nas/a", Err: syscall.EACCES}, true},
		{"full disk still answers reads", &fs.PathError{Op: "write", Path: "/mnt/nas/a", Err: syscall.ENOSPC}, true},
		{"error with no errno is not evidence", errors.New("s3: RequestError: connection reset"), true},
		{"nil", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No Watch: this exercises ReportFailure alone, with no probe loop
			// running that could reach a verdict of its own.
			g := newGate(time.Hour, time.Hour, func(string) error { return nil })
			g.ReportFailure(tt.err)

			ok, cause := g.Healthy()
			if ok != tt.wantHealthy {
				t.Fatalf("Healthy() = %v after ReportFailure(%v), want %v", ok, tt.err, tt.wantHealthy)
			}
			if !ok && cause == nil {
				t.Error("unhealthy with a nil cause")
			}
		})
	}
}

func TestGateOnChangeFiresOnTransitionsOnly(t *testing.T) {
	probed := make(chan struct{}, 64)
	g := newGate(time.Millisecond, testDeadline, func(string) error {
		select {
		case probed <- struct{}{}:
		default:
		}
		return &fs.PathError{Op: "stat", Path: "/mnt/nas", Err: syscall.ESTALE}
	})

	var fired atomic.Int64
	tripped := make(chan struct{}, 1)
	g.SetOnChange(func(healthy bool, _ error) {
		fired.Add(1)
		if !healthy {
			select {
			case tripped <- struct{}{}:
			default:
			}
		}
	})
	g.Watch("/mnt/nas")
	defer g.Stop()

	recv(t, tripped, "the gate to trip")
	// Many more probes, all failing the same way. A subscriber must see one
	// event for the outage, not one per tick.
	for i := 0; i < 20; i++ {
		recv(t, probed, "a further failing probe")
	}
	if n := fired.Load(); n != 1 {
		t.Fatalf("OnChange fired %d times across one outage, want exactly 1", n)
	}
}

func TestGateWatchSwitchesRoots(t *testing.T) {
	roots := make(chan string, 64)
	g := newGate(time.Millisecond, testDeadline, func(root string) error {
		select {
		case roots <- root:
		default:
		}
		return nil
	})
	g.Watch("/mnt/a")
	defer g.Stop()
	waitForRoot(t, roots, "/mnt/a")

	g.Watch("/mnt/b")
	waitForRoot(t, roots, "/mnt/b")

	// The old watcher is stopped before the new one starts, so the old root
	// must stop being probed. Exactly one straggler is tolerated and no more:
	// a probe already in flight when the switch happened outlives its watcher
	// by design, and the capacity-1 semaphore caps that at one.
	stragglers := 0
	for i := 0; i < 20; i++ {
		if root := recv(t, roots, "a probe after the switch"); root != "/mnt/b" {
			stragglers++
			if stragglers > 1 {
				t.Fatalf("probed %q %d times after switching to /mnt/b; the previous watcher was not stopped", root, stragglers)
			}
		}
	}
}

func waitForRoot(t *testing.T, roots <-chan string, want string) {
	t.Helper()
	deadline := time.After(testDeadline)
	for {
		select {
		case got := <-roots:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a probe of %q", want)
		}
	}
}

// TestGateVerdictIsScopedToItsRoot: a verdict is evidence about one path, so
// pointing the gate somewhere else (or nowhere) must not carry it over.
func TestGateVerdictIsScopedToItsRoot(t *testing.T) {
	tests := []struct {
		name string
		act  func(*Gate)
	}{
		{"watching a different root", func(g *Gate) { g.Watch("/mnt/new") }},
		{"stopping, as when the backend becomes s3", func(g *Gate) { g.Stop() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGate(time.Hour, time.Hour, func(string) error { return nil })
			g.ReportFailure(syscall.EIO)
			if ok, _ := g.Healthy(); ok {
				t.Fatal("gate did not trip on an unreachable errno")
			}
			tt.act(g)
			defer g.Stop()
			if ok, cause := g.Healthy(); !ok {
				t.Errorf("Healthy() = (false, %v); a verdict about the old root must not survive %s", cause, tt.name)
			}
		})
	}
}

func TestGateWatchIsIdempotent(t *testing.T) {
	g := newGate(time.Hour, time.Hour, func(string) error { return nil })
	g.Watch("/mnt/a")
	defer g.Stop()

	g.ReportFailure(syscall.EIO)
	// Re-watching the SAME root is a no-op, so it must not quietly clear a
	// verdict that still applies. Every config write calls through here.
	g.Watch("/mnt/a")
	if ok, _ := g.Healthy(); ok {
		t.Fatal("re-watching the same root reset the gate to healthy; Watch must be a no-op when the root is unchanged")
	}
}

// --- the provider wrapper ---

// fakeInner records every call so a test can prove the wrapper did not reach
// it, and returns a fixed error so the reporting path can be driven.
type fakeInner struct {
	mu    sync.Mutex
	calls []string
	err   error

	files []FileInfo
	body  string
	url   string
}

func (f *fakeInner) record(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.err
}

func (f *fakeInner) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeInner) ListFiles(context.Context, string) ([]FileInfo, error) {
	return f.files, f.record("ListFiles")
}

func (f *fakeInner) GetFile(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), f.record("GetFile")
}

func (f *fakeInner) DeletePath(context.Context, string) error { return f.record("DeletePath") }
func (f *fakeInner) CreateDir(context.Context, string) error  { return f.record("CreateDir") }
func (f *fakeInner) CopyToLocal(context.Context, string, string) error {
	return f.record("CopyToLocal")
}

func (f *fakeInner) WriteFile(context.Context, string, io.Reader) error {
	return f.record("WriteFile")
}

func (f *fakeInner) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return f.url, f.record("DownloadURL")
}

func (f *fakeInner) UploadURL(context.Context, string, time.Duration) (string, error) {
	return f.url, f.record("UploadURL")
}

// allProviderCalls drives every StorageProvider method through p.
//
// Named for the interface rather than a count, because the count is what went
// stale: UploadURL joined StorageProvider and this sweep is what makes a new
// method's wrapper behaviour a compile-and-test question rather than something
// the next reader has to notice.
func allProviderCalls(p StorageProvider) []struct {
	name string
	run  func() error
} {
	ctx := context.Background()
	return []struct {
		name string
		run  func() error
	}{
		{"ListFiles", func() error { _, err := p.ListFiles(ctx, "a"); return err }},
		{"GetFile", func() error { _, err := p.GetFile(ctx, "a"); return err }},
		{"DeletePath", func() error { return p.DeletePath(ctx, "a") }},
		{"CreateDir", func() error { return p.CreateDir(ctx, "a") }},
		{"CopyToLocal", func() error { return p.CopyToLocal(ctx, "a", "b") }},
		{"WriteFile", func() error { return p.WriteFile(ctx, "a", strings.NewReader("x")) }},
		{"DownloadURL", func() error { _, err := p.DownloadURL(ctx, "a", time.Minute); return err }},
		{"UploadURL", func() error { _, err := p.UploadURL(ctx, "a", time.Minute); return err }},
	}
}

func TestGatedProviderFailsFastWhenUnhealthy(t *testing.T) {
	inner := &fakeInner{}
	g := newGate(time.Hour, time.Hour, func(string) error { return nil })
	g.ReportFailure(syscall.EIO)
	p := NewGatedProvider(inner, g)

	for _, c := range allProviderCalls(p) {
		t.Run(c.name, func(t *testing.T) {
			err := c.run()
			if !errors.Is(err, ErrBackendUnreachable) {
				t.Errorf("%s error = %v, want it to match ErrBackendUnreachable", c.name, err)
			}
			// A 404 would tell the operator the file is gone when the truth is
			// that Core cannot see the mount.
			if IsNotExist(err) {
				t.Errorf("%s error looks like a missing object: %v", c.name, err)
			}
		})
	}
	if got := inner.recorded(); len(got) != 0 {
		t.Fatalf("inner provider was called %v behind an unhealthy gate, want no calls at all", got)
	}
}

func TestGatedProviderPassesThroughWhenHealthy(t *testing.T) {
	inner := &fakeInner{files: []FileInfo{{Name: "a.jar"}}, body: "payload", url: "https://example.invalid/x"}
	g := newGate(time.Hour, time.Hour, func(string) error { return nil })
	p := NewGatedProvider(inner, g)

	for _, c := range allProviderCalls(p) {
		if err := c.run(); err != nil {
			t.Fatalf("%s through a healthy gate: %v", c.name, err)
		}
	}
	// Counted against the sweep rather than a literal, so adding a method to
	// StorageProvider cannot leave a wrapper silently unforwarded: the sweep
	// grows, this assertion grows with it, and a wrapper that dropped the new
	// call fails here.
	got := inner.recorded()
	if want := len(allProviderCalls(p)); len(got) != want {
		t.Fatalf("inner recorded %v (%d), want all %d StorageProvider methods to reach it", got, len(got), want)
	}

	files, err := p.ListFiles(context.Background(), "")
	if err != nil || len(files) != 1 || files[0].Name != "a.jar" {
		t.Errorf("ListFiles = (%v, %v), want the inner provider's result verbatim", files, err)
	}
	rc, err := p.GetFile(context.Background(), "a.jar")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "payload" {
		t.Errorf("GetFile body = %q, want %q", b, "payload")
	}
}

func TestGatedProviderReportsFailuresToTheGate(t *testing.T) {
	tests := []struct {
		name        string
		innerErr    error
		wantHealthy bool
	}{
		{"unreachable errno trips the gate", &fs.PathError{Op: "open", Path: "/mnt/nas/a", Err: syscall.EHOSTDOWN}, false},
		{"missing object does not", &fs.PathError{Op: "open", Path: "/mnt/nas/a", Err: syscall.ENOENT}, true},
		{"success does not", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &fakeInner{err: tt.innerErr}
			g := newGate(time.Hour, time.Hour, func(string) error { return nil })
			p := NewGatedProvider(inner, g)

			err := p.DeletePath(context.Background(), "a")
			if !errors.Is(err, tt.innerErr) {
				t.Errorf("DeletePath error = %v, want the inner error passed through unchanged (%v)", err, tt.innerErr)
			}
			if ok, _ := g.Healthy(); ok != tt.wantHealthy {
				t.Errorf("Healthy() = %v after the inner provider returned %v, want %v", ok, tt.innerErr, tt.wantHealthy)
			}
		})
	}
}

func TestUnwrapGated(t *testing.T) {
	inner := &fakeInner{}
	if got := UnwrapGated(NewGatedProvider(inner, NewGate())); got != StorageProvider(inner) {
		t.Errorf("UnwrapGated(wrapped) = %T, want the inner provider", got)
	}
	if got := UnwrapGated(inner); got != StorageProvider(inner) {
		t.Errorf("UnwrapGated(unwrapped) = %T, want it returned as-is", got)
	}
}
