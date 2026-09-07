package storage

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

// This file gives the S3 backend a bounded pause-and-retry around the
// operations whose input can be replayed. It is deliberately separate from the
// host-path watchdog in health.go: that one watches a mount with a stat probe
// and fails fast, this one reacts to what the AWS SDK reports about a call that
// already happened. S3 is never gated by Gate and Gate never consults this.
//
// What this buys, stated exactly: an operation that takes only a key or a
// prefix survives a transport outage shorter than the budget, because re-running
// it is equivalent to running it once. That is the whole of it.
//
// What it does NOT buy, because this codebase has shipped comments that claimed
// more than the code could observe:
//   - It does not make S3 reliable. It converts one class of transient failure
//     into a wait, and a wait that runs out still fails.
//   - Uploads do not survive an outage. WriteFile is never retried; see
//     s3ResilientProvider.WriteFile for why retrying it would corrupt data.
//   - Downloads do not survive an outage. Only the GetFile OPEN is retried. The
//     streaming reads happen in the handler, outside this package, and a
//     connection that drops mid-download surfaces to the caller unchanged.
//   - The budget is a ceiling on how long a caller may be made to wait. It is
//     not a promise that the backend comes back within it, or at all.

const (
	// defaultS3RetryInterval is how long the loop waits between attempts.
	defaultS3RetryInterval = 10 * time.Second

	// defaultS3RetryBudget is the ceiling on how long ONE operation may be held
	// waiting before its original error is handed to the caller. It bounds the
	// caller's wait; it says nothing about recovery.
	defaultS3RetryBudget = 10 * time.Minute

	// defaultS3ProbeTimeout bounds ONE probe call. The probe runs inline in the
	// loop, so a probe that outlives this would only delay the next tick, never
	// pile up: the ticker drops ticks rather than queueing them.
	defaultS3ProbeTimeout = 10 * time.Second
)

// ErrS3ProbeUnavailable is what a probe function returns when the probe could
// not be ATTEMPTED: no s3 backend is configured, or the client could not be
// built from the current config. It is evidence about the configuration, not
// about the connection, so the loop records no verdict for it.
//
// The distinction is load-bearing. Without it, an owner who breaks the s3
// config in the middle of an outage would have the resulting build error
// classified as "not a connection failure", which the loop reads as the store
// answering, which would clear the reconnecting state on no evidence at all.
var ErrS3ProbeUnavailable = errors.New("s3 connection probe could not be attempted")

// s3Transition names the three ways this state machine moves, because they are
// not interchangeable in the log. "Restored" is a claim that the backend came
// back and must only be made when something observed that; "abandoned" is the
// state being dropped because the configured backend is no longer this one,
// which is not evidence about anything.
type s3Transition int

const (
	s3Lost s3Transition = iota
	s3Restored
	s3Abandoned
)

// s3ConnectionClass is the classifier, composed from the AWS SDK's own retry
// taxonomy rather than a hand-written list of error strings.
//
// The order mirrors retry.NewStandard's own Retryables and the ordering is
// load-bearing: IsErrorRetryables returns the first non-Unknown verdict, so
// NoRetryCanceledError must come first to veto a cancelled request before
// RetryableConnectionError can call it a timeout and retry it.
//
// retry.RetryableError and retry.RetryableHTTPStatusCode are deliberately NOT
// in this list even though the SDK's standard retryer uses them. They cover
// throttling and 5xx service replies, which are the service ANSWERING, not the
// connection failing. Those are the SDK's own per-call retry budget to spend,
// and pausing the whole backend for ten minutes over a SlowDown would be the
// same over-reaction as pausing it over a 403.
var s3ConnectionClass = retry.IsErrorRetryables{
	retry.NoRetryCanceledError{},
	retry.RetryableConnectionError{},
}

// isS3ConnectionClass reports whether err is evidence the connection to the
// object store failed, as opposed to the store answering with a refusal.
//
// A 403, a NoSuchKey and anything else isS3NotFound recognises all fall to the
// SDK's Unknown verdict and therefore return false here. That is the point:
// waiting ten minutes to re-confirm a permission error is a bug, not resilience.
//
// The context check runs first and is not redundant. context.DeadlineExceeded
// implements both Timeout() and Temporary() returning true (verified in the
// stdlib, context.go), so RetryableConnectionError's generic temporary/timeout
// fallback classifies a bare one as retryable. A caller whose own deadline
// expired is not evidence about the backend, and treating it as an outage would
// flip every S3 provider in the process to reconnecting over one slow request.
func isS3ConnectionClass(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return s3ConnectionClass.IsErrorRetryable(err) == aws.TrueTernary
}

// s3State is swapped atomically so State never blocks behind a transition.
type s3State struct {
	reconnecting bool
	// lastErr is nil when ok and always non-nil when reconnecting.
	lastErr error
	// since is when the CURRENT state was entered.
	since time.Time
}

// S3Resilience holds the connection state for one S3 backend and runs the
// retry loop. One instance exists per Core, long-lived, because core-storage
// providers are rebuilt on every request: state kept on the provider would
// reset before it could ever be read.
//
// The zero value is not usable; build it with NewS3Resilience. Every method is
// safe on a nil receiver, which is what lets a caller constructed without one
// (tests, the connection probe, the migration's candidate-config build) share a
// single code path with the wired-up one.
type S3Resilience struct {
	// stateMu serialises transitions so two of them cannot interleave and fire
	// onChange, or log, out of order. state is read without it.
	stateMu  sync.Mutex
	state    atomic.Pointer[s3State]
	onChange func(reconnecting bool, since time.Time, lastErr error)

	// interval and budget live here rather than as bare consts so a test can
	// shrink them; nothing waits 10s or 10min in the test suite.
	interval time.Duration
	budget   time.Duration

	// probeTimeout bounds one probe call. Same reason as above for being a
	// field: the suite shrinks it.
	probeTimeout time.Duration

	// logf is injected so a test can capture the log sink and count lines. It
	// defaults to log.Printf, which is this module's logging idiom.
	logf func(format string, args ...any)

	// now is injected so a budget test does not depend on wall-clock time.
	now func() time.Time
}

// NewS3Resilience builds the production instance: 10s between attempts, a
// 10-minute ceiling on any one operation's wait.
//
// It starts in the ok state. Starting reconnecting would make a fresh boot pause
// every S3 operation until something proved the backend was up, and nothing
// here proves that except a successful call.
func NewS3Resilience() *S3Resilience {
	return newS3Resilience(defaultS3RetryInterval, defaultS3RetryBudget)
}

func newS3Resilience(interval, budget time.Duration) *S3Resilience {
	r := &S3Resilience{
		interval:     interval,
		budget:       budget,
		probeTimeout: defaultS3ProbeTimeout,
		logf:         log.Printf,
		now:          time.Now,
	}
	r.state.Store(&s3State{since: time.Now()})
	return r
}

// SetOnChange installs a hook fired on TRANSITIONS only, never per retry, so a
// subscriber emits one event per state change. It is called with no locks held,
// so a hook may call back in.
//
// services.StorageStatus installs the one production hook, which forwards the
// transition onto the system-events channel.
func (r *S3Resilience) SetOnChange(fn func(reconnecting bool, since time.Time, lastErr error)) {
	if r == nil {
		return
	}
	r.stateMu.Lock()
	r.onChange = fn
	r.stateMu.Unlock()
}

// State reports whether the backend is currently in the reconnecting state,
// when it entered it, and the error that put it there. It reads an atomic and
// never touches the network.
//
// Not-reconnecting means "no operation has reported a connection failure that
// has yet to recover". It is NOT a verified-reachable signal: nothing probes S3
// on a timer, so a backend can be down and unobserved until something calls it.
func (r *S3Resilience) State() (reconnecting bool, since time.Time, lastErr error) {
	if r == nil {
		return false, time.Time{}, nil
	}
	s := r.state.Load()
	return s.reconnecting, s.since, s.lastErr
}

// applyState records a transition and returns the log line and hook call it
// owes, or nil when nothing changed. Splitting the side effects out is what
// keeps them off the lock and what makes "logged once" a property of the state
// machine rather than of a flag somebody has to remember to check.
func (r *S3Resilience) applyState(t s3Transition, cause error) func() {
	reconnecting := t == s3Lost

	r.stateMu.Lock()
	defer r.stateMu.Unlock()

	prev := r.state.Load()
	if prev.reconnecting == reconnecting {
		return nil
	}
	now := r.now()
	r.state.Store(&s3State{reconnecting: reconnecting, lastErr: cause, since: now})

	hook, logf := r.onChange, r.logf
	outage := now.Sub(prev.since)
	return func() {
		switch t {
		case s3Lost:
			logf("core storage s3: connection lost, pausing replayable operations and retrying every %s for up to %s: %v", r.interval, r.budget, cause)
		case s3Restored:
			logf("core storage s3: connection restored after %s", outage.Round(time.Second))
		case s3Abandoned:
			// Deliberately NOT the "restored" line. Nothing observed a
			// recovery here; the configured backend simply stopped being this
			// one, and claiming the connection came back would be a log entry
			// asserting more than the code can see.
			logf("core storage s3: backend is no longer s3, dropping the reconnecting state after %s", outage.Round(time.Second))
		}
		if hook != nil {
			hook(reconnecting, now, cause)
		}
	}
}

func (r *S3Resilience) fire(fn func()) {
	if fn != nil {
		fn()
	}
}

// report records a connection-class failure. Anything else is ignored: a
// missing object or a permission refusal must never put the backend into
// reconnecting, because the connection plainly worked.
func (r *S3Resilience) report(err error) {
	if r == nil || !isS3ConnectionClass(err) {
		return
	}
	r.fire(r.applyState(s3Lost, err))
}

// Reset drops the reconnecting state because the configured backend is no
// longer s3. Called from the same place that starts and stops the host-path
// watchdog, so exactly one of the two mechanisms is ever live.
//
// Without this a Core that was reconnecting when its backend was switched to a
// host path would report an s3 outage forever: nothing else can clear the state
// except a successful s3 call, and there will never be another one. That stale
// verdict reaches the panel banner and both health endpoints.
func (r *S3Resilience) Reset() {
	if r == nil {
		return
	}
	r.fire(r.applyState(s3Abandoned, nil))
}

// recovered records that a call completed, which is the only evidence this
// package ever has that the backend is up.
//
// The atomic read short-circuits before the transition lock because this runs on
// every successful storage operation, and taking a mutex on that path to
// discover there is nothing to do would be a needless contention point. Racing
// it is harmless: applyState re-checks under the lock, and losing the race means
// a concurrent failure just recorded newer evidence than this success.
func (r *S3Resilience) recovered() {
	if r == nil || !r.state.Load().reconnecting {
		return
	}
	r.fire(r.applyState(s3Restored, nil))
}

// StartProbe runs the recovery probe until ctx is done. It spawns its own
// goroutine and returns immediately.
//
// probe must perform ONE cheap, read-only, replayable call against the
// currently configured s3 backend, and it must NOT go through the resilient
// wrapper: a probe that did would be caught by s3Retry and sit there for the
// whole budget instead of reporting a verdict.
//
// This is what makes the write path's wait active. Without it the state only
// returns to ok when some other replayable operation happens to succeed, so a
// Core doing nothing but uploads would wait out the full budget without ever
// noticing that the backend came back.
//
// Unlike the host-path watchdog, this runs the probe INLINE and relies on a
// context deadline to bound it. That looks like the opposite of the rule in
// health.go and is deliberate: the rule there is about filesystem syscalls,
// which no context can interrupt, so a timeout around one only abandons a
// goroutine that stays pinned to an OS thread. The AWS SDK is context-aware
// and genuinely aborts, so here the deadline is a real bound and there is
// nothing to detach.
func (r *S3Resilience) StartProbe(ctx context.Context, probe func(context.Context) error) {
	if r == nil || probe == nil {
		return
	}
	go r.probeLoop(ctx, probe)
}

func (r *S3Resilience) probeLoop(ctx context.Context, probe func(context.Context) error) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Only probes during an outage. A healthy backend needs no traffic
			// generated on its behalf, and every real call is already evidence.
			if !r.state.Load().reconnecting {
				continue
			}
			r.runProbe(ctx, probe)
		}
	}
}

// runProbe performs one probe and records the verdict it justifies.
func (r *S3Resilience) runProbe(ctx context.Context, probe func(context.Context) error) {
	pctx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()
	err := probe(pctx)

	// Checked on pctx rather than by classifying err, and that is load-bearing.
	// isS3ConnectionClass returns false for a context deadline, so a probe that
	// merely ran out of time would fall through to the branch below and be read
	// as the store answering, clearing the outage on no evidence whatsoever.
	// Asking the context directly is also immune to how the SDK wrapped it.
	if pctx.Err() != nil {
		return
	}
	if errors.Is(err, ErrS3ProbeUnavailable) {
		return
	}
	if err == nil || !isS3ConnectionClass(err) {
		// Either the call succeeded, or the store answered with a refusal
		// (403, NoSuchKey). Both mean the transport works, which is the only
		// thing this state tracks. s3Retry treats a refusal the same way.
		r.recovered()
	}
}

// waitUntilOK blocks while the backend is reconnecting and returns once it is
// not, bounded three ways: ctx, the budget measured from when the outage began,
// and the state itself.
//
// It is the WRITE path's share of this mechanism. It runs before the caller's
// reader is touched, which is what makes it safe for an operation that must
// never be re-run.
//
// This wait is passive on its own: it never probes. What makes it terminate on
// recovery rather than on the budget is StartProbe running alongside it, which
// is the only thing that clears the state for a Core whose sole storage traffic
// is uploads. Without that loop wired up, this waits out the full budget.
func (r *S3Resilience) waitUntilOK(ctx context.Context) error {
	if r == nil {
		return nil
	}
	for {
		s := r.state.Load()
		if !s.reconnecting {
			return nil
		}
		if r.now().Sub(s.since) >= r.budget {
			return s.lastErr
		}
		if err := sleepCtx(ctx, r.interval); err != nil {
			return err
		}
	}
}

// sleepCtx waits for d, or returns ctx's error as soon as it is done. The timer
// is always stopped, so a cancelled wait does not leave one armed.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// s3Retry runs fn, and on a connection-class failure pauses and re-runs it
// until it succeeds, the budget runs out, or ctx is done.
//
// ONLY call this with an fn whose input can be replayed, meaning it carries
// nothing but a key or a prefix. Re-running anything that consumes a
// caller-owned reader corrupts data; see s3ResilientProvider.WriteFile.
//
// On budget expiry the caller gets the ORIGINAL error, not a synthetic
// deadline one, because the first failure is what actually went wrong and the
// identical ones after it add nothing. The state stays reconnecting: the budget
// ran out for this operation, which is not evidence the backend recovered.
//
// It is a free function rather than a method because Go has no
// type-parameterized methods; doValue in limiter.go has the same shape.
func s3Retry[T any](r *S3Resilience, ctx context.Context, fn func() (T, error)) (T, error) {
	val, err := fn()
	if err == nil {
		r.recovered()
		return val, nil
	}
	if r == nil || !isS3ConnectionClass(err) {
		// A refusal (403, NoSuchKey) or a cancelled caller. Propagate it
		// untouched and leave the state alone.
		return val, err
	}

	first := err
	r.fire(r.applyState(s3Lost, first))
	deadline := r.now().Add(r.budget)
	for {
		if werr := sleepCtx(ctx, r.interval); werr != nil {
			return val, werr
		}
		if !r.now().Before(deadline) {
			var zero T
			return zero, first
		}
		val, err = fn()
		if err == nil {
			r.recovered()
			return val, nil
		}
		if !isS3ConnectionClass(err) {
			// The store answered, with a refusal. That is recovery evidence for
			// the CONNECTION even though this call failed, so clear the state
			// and hand the refusal on.
			r.recovered()
			return val, err
		}
	}
}

// s3ResilientProvider applies S3Resilience to an S3Provider. The split between
// the methods that retry and the two that only wait is the whole design; it is
// explained on each.
type s3ResilientProvider struct {
	inner StorageProvider
	res   *S3Resilience
}

// NewS3ResilientProvider wraps inner so its replayable operations pause and
// retry across a transport outage. Used for the s3 backend only; the host path
// has its own, different mechanism in health.go.
func NewS3ResilientProvider(inner StorageProvider, res *S3Resilience) StorageProvider {
	return &s3ResilientProvider{inner: inner, res: res}
}

func (p *s3ResilientProvider) ListFiles(ctx context.Context, path string) ([]FileInfo, error) {
	return s3Retry(p.res, ctx, func() ([]FileInfo, error) {
		return p.inner.ListFiles(ctx, path)
	})
}

// GetFile retries the OPEN only. Everything after it is streamed by the handler,
// outside this package, so a connection that drops mid-download is not retried
// and reaches the caller as it is.
func (p *s3ResilientProvider) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	return s3Retry(p.res, ctx, func() (io.ReadCloser, error) {
		return p.inner.GetFile(ctx, path)
	})
}

func (p *s3ResilientProvider) DeletePath(ctx context.Context, path string) error {
	_, err := s3Retry(p.res, ctx, func() (struct{}, error) {
		return struct{}{}, p.inner.DeletePath(ctx, path)
	})
	return err
}

func (p *s3ResilientProvider) CreateDir(ctx context.Context, path string) error {
	_, err := s3Retry(p.res, ctx, func() (struct{}, error) {
		return struct{}{}, p.inner.CreateDir(ctx, path)
	})
	return err
}

func (p *s3ResilientProvider) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s3Retry(p.res, ctx, func() (string, error) {
		return p.inner.DownloadURL(ctx, key, ttl)
	})
}

// UploadURL is retried like DownloadURL and unlike WriteFile: presigning is a
// local signature computation over the key, so a second attempt repeats no
// side effect and consumes no reader.
func (p *s3ResilientProvider) UploadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s3Retry(p.res, ctx, func() (string, error) {
		return p.inner.UploadURL(ctx, key, ttl)
	})
}

// WriteFile waits for a reconnecting backend to come back BEFORE it starts, and
// is then run exactly once. It is never retried, and that is a correctness
// requirement rather than a tuning choice.
//
// objectStore.Put takes a non-seekable io.Reader. By the time an error comes
// back some of that reader has already been consumed, so re-running the call
// would upload whatever is LEFT of the stream as if it were the whole object: a
// silently truncated file under the correct name. Some call sites do happen to
// pass a seekable body, but the interface does not promise one and this layer
// cannot tell, so the guarantee has to hold for the weakest case.
//
// Waiting first is free of that problem because it happens before the reader is
// touched. Once the first byte is read the call is committed and any failure
// propagates.
func (p *s3ResilientProvider) WriteFile(ctx context.Context, path string, content io.Reader) error {
	if err := p.res.waitUntilOK(ctx); err != nil {
		return err
	}
	err := p.inner.WriteFile(ctx, path, content)
	// Still worth reporting: recording the outage is not the same as retrying
	// it, and it is what lets the NEXT replayable call, and step 6's event,
	// know the backend is down.
	if err != nil {
		p.res.report(err)
		return err
	}
	p.res.recovered()
	return nil
}

// CopyToLocal waits but does not retry, for a different reason than WriteFile.
// Its input is replayable, so a retry would be safe, but it streams whole object
// bodies to local files and can move gigabytes. Re-running that from the start
// on a 10-second timer is not a trade this step is willing to make on its own,
// and the design's retried list does not include it.
func (p *s3ResilientProvider) CopyToLocal(ctx context.Context, srcPath, destPath string) error {
	if err := p.res.waitUntilOK(ctx); err != nil {
		return err
	}
	err := p.inner.CopyToLocal(ctx, srcPath, destPath)
	if err != nil {
		p.res.report(err)
		return err
	}
	p.res.recovered()
	return nil
}
