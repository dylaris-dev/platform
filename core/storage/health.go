package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// This file holds the watchdog for the host-path backend and the provider
// wrapper that consults it. S3 is deliberately not gated: its failures are the
// SDK's to judge and it has its own resilience story.
//
// What this is for. A host path that is a network share can stop answering.
// Every request that touches it then blocks inside a syscall that no context
// can interrupt. Each blocked request pins an OS thread, and Go's default
// 10,000-thread ceiling is a fatal, unrecoverable crash when it is exceeded.
//
// What this does NOT do, because the difference matters and comments in this
// codebase have oversold guards before: it does not rescue a request that is
// already blocked inside a syscall. Those stay blocked until the kernel gives
// up, and on NFS mounted "hard" they stay blocked until the server returns.
// This bounds the PILE-UP behind that first wave. It reduces the odds of
// thread exhaustion; it does not remove them, and it guarantees no
// availability of any kind.

const (
	// defaultProbeInterval is how often the watcher looks at the root.
	defaultProbeInterval = 10 * time.Second

	// defaultProbeTimeout is how long a single probe may take before the
	// mount is treated as not answering. It is well under the interval so a
	// verdict is always reached before the next tick.
	defaultProbeTimeout = 5 * time.Second
)

// ErrBackendUnreachable is returned by a gated provider, and by a gated
// provider build, when the watchdog has evidence the host path is not
// answering. Callers match it with errors.Is to surface a distinguishable
// failure rather than a generic error.
//
// It deliberately does NOT wrap fs.ErrNotExist, so IsNotExist can never report
// true for it. Answering "not found" here would tell an operator the file is
// gone when the truth is that Core cannot see the mount.
var ErrBackendUnreachable = errors.New("storage backend is unreachable")

// errProbeTimedOut and errProbeStillRunning are the two causes the watcher
// itself produces. Neither carries an errno, because neither came from a
// syscall that returned.
var (
	errProbeTimedOut     = errors.New("storage probe did not return within the timeout")
	errProbeStillRunning = errors.New("a previous storage probe has not returned")
)

// gateState is swapped atomically so Healthy never blocks behind a transition.
type gateState struct {
	healthy bool
	// cause is nil when healthy and always non-nil when not.
	cause error
}

// Gate is the host-path watchdog and circuit breaker. One instance exists per
// Core, watching whichever single root the core-storage config names.
//
// The zero value is not usable; build it with NewGate. Every method is safe on
// a nil receiver, which is what lets callers that were constructed without a
// gate (tests, and the migration's candidate-config builds) share one code
// path with the wired-up one.
type Gate struct {
	// mu guards the watcher lifecycle only: root, cancel and done.
	mu     sync.Mutex
	root   string
	cancel context.CancelFunc
	done   chan struct{}

	// state is the answer Healthy hands out. stateMu serialises transitions so
	// two of them (a probe verdict and a ReportFailure) cannot interleave and
	// fire onChange out of order.
	stateMu  sync.Mutex
	state    atomic.Pointer[gateState]
	onChange func(healthy bool, cause error)

	// sem has capacity 1. It is acquired before a probe goroutine starts and
	// released inside it once the stat has returned, which caps outstanding
	// probes at exactly one. Without it a wedged mount would abandon one
	// goroutine per tick forever instead of one in total.
	sem chan struct{}

	// fsSem bounds how many goroutines may be inside a host-path filesystem
	// syscall at once, and fsDeadline is how long a detached one may run before
	// its caller gives up on it. Both live here rather than as bare consts so a
	// test can shrink them; see limiter.go for what they are for.
	fsSem      chan struct{}
	fsDeadline time.Duration

	interval time.Duration
	timeout  time.Duration
	// probe is injected so tests can drive a stat that hangs, one that errors
	// and one that succeeds without needing a real broken mount.
	probe func(root string) error
}

// NewGate builds a watchdog with the production interval, timeout and probe.
//
// It starts HEALTHY. A gate that started unhealthy would refuse every request
// on a fresh boot until the first probe landed. Unhealthy is a state the
// system has to be argued into, by evidence.
func NewGate() *Gate {
	return newGate(defaultProbeInterval, defaultProbeTimeout, statProbe)
}

func newGate(interval, timeout time.Duration, probe func(root string) error) *Gate {
	g := &Gate{
		sem:        make(chan struct{}, 1),
		fsSem:      make(chan struct{}, MaxConcurrentFSOps),
		fsDeadline: fsOpDeadline,
		interval:   interval,
		timeout:    timeout,
		probe:      probe,
	}
	g.state.Store(&gateState{healthy: true})
	return g
}

// statProbe is the production probe: the cheapest call that still has to reach
// the filesystem, and therefore the one that hangs when the mount does.
func statProbe(root string) error {
	_, err := os.Stat(root)
	return err
}

// SetOnChange installs a hook fired on TRANSITIONS only, never on every tick,
// so a subscriber can emit one event per state change. It is called without
// any of the gate's locks held, so a hook may call back into the gate.
func (g *Gate) SetOnChange(fn func(healthy bool, cause error)) {
	if g == nil {
		return
	}
	g.stateMu.Lock()
	g.onChange = fn
	g.stateMu.Unlock()
}

// Healthy reports whether the gate currently has evidence of a problem, and
// the cause when it does. It reads an atomic and never touches the filesystem.
//
// True means "no evidence of a problem", NOT "verified reachable". The mount
// can be dead and unproven for up to one probe interval, and a caller must not
// read a true here as a promise that the next call will succeed.
func (g *Gate) Healthy() (bool, error) {
	if g == nil {
		return true, nil
	}
	s := g.state.Load()
	return s.healthy, s.cause
}

// ReportFailure trips the gate when err says the backend itself is unusable,
// without waiting for the next probe. Anything IsBackendUnreachable does not
// recognise is ignored: a missing object, a permission problem or a full disk
// must never take the whole backend offline.
func (g *Gate) ReportFailure(err error) {
	if g == nil || !IsBackendUnreachable(err) {
		return
	}
	g.setHealth(false, err)
}

// Watch starts (or moves) the watcher. It is idempotent: watching the root
// already being watched does nothing. A DIFFERENT root stops the previous
// watcher first and resets the state to healthy, because evidence gathered
// about the old path says nothing about the new one. Exactly one watcher
// exists at a time, which is right because exactly one core-storage config
// exists at a time.
func (g *Gate) Watch(root string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.cancel != nil && g.root == root {
		g.mu.Unlock()
		return
	}
	g.stopLocked()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	g.root, g.cancel, g.done = root, cancel, done
	// Reset before the loop starts, so the incoming watcher cannot have its
	// first verdict overwritten by the reset that belongs to the old root.
	fire := g.applyHealth(true, nil)
	go g.loop(ctx, root, done)
	g.mu.Unlock()
	fireChange(fire)
}

// Stop ends the watcher and returns the gate to healthy. Called when the
// configured backend is no longer a host path: a stale unhealthy verdict about
// an abandoned path must not keep refusing requests that no longer go there.
func (g *Gate) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.stopLocked()
	fire := g.applyHealth(true, nil)
	g.mu.Unlock()
	fireChange(fire)
}

// stopLocked cancels the watcher and waits for it to actually exit, so a
// verdict from the outgoing loop can never land on the incoming one's state.
// The loop selects on ctx.Done everywhere it waits, so this cannot block on a
// stuck probe: the abandoned probe goroutine outlives the loop by design.
func (g *Gate) stopLocked() {
	if g.cancel == nil {
		return
	}
	g.cancel()
	<-g.done
	g.root, g.cancel, g.done = "", nil, nil
}

func (g *Gate) loop(ctx context.Context, root string, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.runProbe(ctx, root)
		}
	}
}

// runProbe stats root in a throwaway goroutine and waits on it with a timeout.
func (g *Gate) runProbe(ctx context.Context, root string) {
	select {
	case g.sem <- struct{}{}:
	default:
		// The previous probe has still not come back, so the mount is still
		// not answering. Skipping is the point: it is what keeps the number of
		// abandoned goroutines at one instead of one per tick.
		g.setHealth(false, errProbeStillRunning)
		return
	}

	// Buffered, and this is load-bearing. The waiter below abandons this
	// channel on timeout; on an unbuffered one the probe goroutine would then
	// block on the send forever even after the syscall finally returned,
	// turning a temporary outage into a permanent goroutine leak.
	res := make(chan error, 1)
	go func() {
		err := g.probe(root)
		res <- err
		<-g.sem
	}()

	timer := time.NewTimer(g.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		g.setHealth(false, errProbeTimedOut)
	case err := <-res:
		// A probe that returned at all means the filesystem answered, so
		// anything the classifier does not call an outage counts as recovery
		// evidence. In particular a root that does not exist yet must not trip
		// the gate: on a fresh install nothing has created it.
		if IsBackendUnreachable(err) {
			g.setHealth(false, err)
			return
		}
		g.setHealth(true, nil)
	}
}

func (g *Gate) setHealth(healthy bool, cause error) {
	fireChange(g.applyHealth(healthy, cause))
}

// applyHealth records a transition and returns the onChange call it owes, or
// nil when nothing changed. Splitting the call out lets Watch and Stop update
// the state while holding g.mu and still run the hook with every lock
// released, so a hook is free to call back into the gate.
func (g *Gate) applyHealth(healthy bool, cause error) func() {
	if healthy {
		cause = nil
	} else if cause == nil {
		cause = ErrBackendUnreachable
	}

	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	if g.state.Load().healthy == healthy {
		return nil
	}
	g.state.Store(&gateState{healthy: healthy, cause: cause})
	hook := g.onChange
	if hook == nil {
		return nil
	}
	return func() { hook(healthy, cause) }
}

func fireChange(fn func()) {
	if fn != nil {
		fn()
	}
}

// gateBlockedError renders the gate's verdict as an error callers can match.
//
// The cause is interpolated with %v rather than wrapped, so no errno inside it
// can leak out through errors.Is and make this look like something it is not.
func gateBlockedError(cause error) error {
	if cause == nil {
		return ErrBackendUnreachable
	}
	return fmt.Errorf("%w: %v", ErrBackendUnreachable, cause)
}

// GatedProviderBlocked returns a non-nil error when gate has evidence the host
// path is not answering. Call it before doing filesystem work that a wedged
// mount would block on, including the work done to CONSTRUCT a provider.
func GatedProviderBlocked(gate *Gate) error {
	if ok, cause := gate.Healthy(); !ok {
		return gateBlockedError(cause)
	}
	return nil
}

// gatedProvider checks the gate before every call, runs the inner call under
// the gate's concurrency bound, and feeds whatever the inner provider reports
// back into the gate.
//
// The fail-fast is a bound on new work reaching a mount that is known not to
// answer. It cannot help a call already inside a syscall, and it is not a
// promise that a call which passes the check will complete. What the bound adds
// is that the number of calls stuck in that state is a constant rather than a
// function of traffic; see limiter.go.
//
// The split between the detached calls below and the two that run inline is
// deliberate and is explained on Gate.Run.
type gatedProvider struct {
	inner StorageProvider
	gate  *Gate
}

// NewGatedProvider wraps inner so every call consults gate first. Used for the
// host-path backend only.
func NewGatedProvider(inner StorageProvider, gate *Gate) StorageProvider {
	return &gatedProvider{inner: inner, gate: gate}
}

// UnwrapGated returns the provider underneath a gated wrapper, or p itself
// when it is not wrapped. Callers that need to inspect the concrete backend
// (its root, its bucket) must go through this, because wrapping otherwise
// hides the type behind an interface.
func UnwrapGated(p StorageProvider) StorageProvider {
	if gp, ok := p.(*gatedProvider); ok {
		return gp.inner
	}
	return p
}

func (p *gatedProvider) blocked() error { return GatedProviderBlocked(p.gate) }

func (p *gatedProvider) observe(err error) error {
	p.gate.ReportFailure(err)
	return err
}

func (p *gatedProvider) ListFiles(ctx context.Context, path string) ([]FileInfo, error) {
	if err := p.blocked(); err != nil {
		return nil, err
	}
	files, err := doValue(p.gate, ctx, func() ([]FileInfo, error) {
		return p.inner.ListFiles(ctx, path)
	}, nil)
	return files, p.observe(err)
}

func (p *gatedProvider) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := p.blocked(); err != nil {
		return nil, err
	}
	// The returned ReadCloser keeps its slot until Close, because the streaming
	// reads happen in the handler where nothing else here can see them.
	rc, err := openDetached(p.gate, ctx, func() (io.ReadCloser, error) {
		return p.inner.GetFile(ctx, path)
	})
	return rc, p.observe(err)
}

func (p *gatedProvider) DeletePath(ctx context.Context, path string) error {
	if err := p.blocked(); err != nil {
		return err
	}
	return p.observe(p.gate.Do(ctx, func() error {
		return p.inner.DeletePath(ctx, path)
	}))
}

func (p *gatedProvider) CreateDir(ctx context.Context, path string) error {
	if err := p.blocked(); err != nil {
		return err
	}
	return p.observe(p.gate.Do(ctx, func() error {
		return p.inner.CreateDir(ctx, path)
	}))
}

func (p *gatedProvider) CopyToLocal(ctx context.Context, srcPath, destPath string) error {
	if err := p.blocked(); err != nil {
		return err
	}
	return p.observe(p.gate.Run(ctx, func() error {
		return p.inner.CopyToLocal(ctx, srcPath, destPath)
	}))
}

func (p *gatedProvider) WriteFile(ctx context.Context, path string, content io.Reader) error {
	if err := p.blocked(); err != nil {
		return err
	}
	return p.observe(p.gate.Run(ctx, func() error {
		return p.inner.WriteFile(ctx, path, content)
	}))
}

func (p *gatedProvider) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := p.blocked(); err != nil {
		return "", err
	}
	url, err := doValue(p.gate, ctx, func() (string, error) {
		return p.inner.DownloadURL(ctx, key, ttl)
	}, nil)
	return url, p.observe(err)
}

func (p *gatedProvider) UploadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := p.blocked(); err != nil {
		return "", err
	}
	url, err := doValue(p.gate, ctx, func() (string, error) {
		return p.inner.UploadURL(ctx, key, ttl)
	}, nil)
	return url, p.observe(err)
}
