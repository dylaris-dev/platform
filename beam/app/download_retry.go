package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errDownloadCancelled is the result of a download the user stopped. The page
// marks the entry itself, so the text only has to be recognisable.
var errDownloadCancelled = errors.New("cancelled")

// downloadDialError is a failure to reach the node for a retry. Always worth
// another try: it is the network or Core that failed, not the request.
type downloadDialError struct{ err error }

func (e *downloadDialError) Error() string { return "reconnect: " + e.err.Error() }
func (e *downloadDialError) Unwrap() error { return e.err }

// retryableDownloadErr reports whether a failed attempt may succeed on a fresh
// connection. Anything that is not a gRPC status is local (the disk is full,
// the folder is gone) and would fail the same way again. Of the statuses, the
// ones a dropped or replaced tunnel produces qualify; NotFound, PermissionDenied,
// ResourceExhausted (a quota) and Internal are an answer, not an accident.
func retryableDownloadErr(err error) bool {
	var dial *downloadDialError
	if errors.Is(err, errDownloadStalled) || errors.As(err, &dial) {
		return true
	}
	s, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch s.Code() {
	case codes.Unavailable, codes.Unknown, codes.DeadlineExceeded, codes.Aborted, codes.Canceled, codes.Unauthenticated:
		return true
	}
	return false
}

// downloadRetry decides how long a broken download keeps trying. The window is
// measured from the last attempt that moved bytes, so a link that drops every
// few minutes but works in between is never given up on, and one that stays
// down is given up on after window.
type downloadRetry struct {
	window     time.Duration
	minBackoff time.Duration
	maxBackoff time.Duration
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
}

var defaultDownloadRetry = downloadRetry{
	window:     15 * time.Minute,
	minBackoff: 2 * time.Second,
	maxBackoff: time.Minute,
	now:        time.Now,
	sleep: func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	},
}

// run calls attempt (n counts from 1) until it succeeds, fails for good, the
// window runs out, or ctx is cancelled. attempt reports whether it received
// any bytes before failing. onRetry hears about each retry before its wait.
func (p downloadRetry) run(ctx context.Context, attempt func(ctx context.Context, n int) (progressed bool, err error), onRetry func(n int, err error)) error {
	lastProgress := p.now()
	backoff := p.minBackoff
	for n := 1; ; n++ {
		progressed, err := attempt(ctx, n)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errDownloadCancelled
		}
		if !retryableDownloadErr(err) {
			return err
		}
		if progressed {
			lastProgress = p.now()
			backoff = p.minBackoff
		}
		if p.now().Add(backoff).Sub(lastProgress) > p.window {
			return fmt.Errorf("gave up after %s without progress: %w", p.window, err)
		}
		onRetry(n+1, err)
		if p.sleep(ctx, backoff) != nil {
			return errDownloadCancelled
		}
		backoff = min(backoff*2, p.maxBackoff)
	}
}
