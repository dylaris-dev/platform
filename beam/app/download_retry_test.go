package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeRetry is downloadRetry on a fake clock that a backoff sleep advances.
func fakeRetry() (*downloadRetry, *[]time.Duration) {
	p, waits, _ := fakeRetryClock()
	return p, waits
}

// fakeRetryClock also returns a way to pass time inside an attempt.
func fakeRetryClock() (*downloadRetry, *[]time.Duration, func(time.Duration)) {
	at := time.Unix(0, 0)
	var waits []time.Duration
	p := &downloadRetry{
		window:     15 * time.Minute,
		minBackoff: 2 * time.Second,
		maxBackoff: time.Minute,
		now:        func() time.Time { return at },
		sleep: func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			at = at.Add(d)
			return ctx.Err()
		},
	}
	return p, &waits, func(d time.Duration) { at = at.Add(d) }
}

func TestRetryableDownloadErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{status.Error(codes.Unavailable, "transport is closing"), true},
		{status.Error(codes.Canceled, "context canceled"), true},
		{status.Error(codes.Unauthenticated, "ticket expired"), true},
		{fmt.Errorf("%w: no data received for 5m0s", errDownloadStalled), true},
		{&downloadDialError{errors.New("dial tcp: i/o timeout")}, true},
		{status.Error(codes.NotFound, "file not found"), false},
		{status.Error(codes.PermissionDenied, "no"), false},
		{status.Error(codes.ResourceExhausted, "daily quota"), false},
		{status.Error(codes.Internal, "zip stream: read error"), false},
		// A local write error: the disk is full, and stays full.
		{errors.New("write C:\\x.zip: There is not enough space on the disk."), false},
	}
	for _, c := range cases {
		if got := retryableDownloadErr(c.err); got != c.want {
			t.Errorf("retryableDownloadErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestDownloadRetryRecoversAfterDrops(t *testing.T) {
	p, waits := fakeRetry()
	var retried []int
	calls := 0
	err := p.run(context.Background(), func(_ context.Context, n int) (bool, error) {
		calls++
		if n < 4 {
			return false, status.Error(codes.Unavailable, "down")
		}
		return true, nil
	}, func(n int, _ error) { retried = append(retried, n) })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || fmt.Sprint(retried) != "[2 3 4]" {
		t.Fatalf("calls=%d retried=%v", calls, retried)
	}
	if fmt.Sprint(*waits) != "[2s 4s 8s]" {
		t.Fatalf("backoff %v, want doubling from 2s", *waits)
	}
}

func TestDownloadRetryGivesUpAfterWindowWithoutProgress(t *testing.T) {
	p, waits := fakeRetry()
	calls := 0
	err := p.run(context.Background(), func(context.Context, int) (bool, error) {
		calls++
		return false, status.Error(codes.Unavailable, "down")
	}, func(int, error) {})
	if err == nil || status.Code(errors.Unwrap(err)) != codes.Unavailable {
		t.Fatalf("err = %v, want the last failure wrapped", err)
	}
	var total time.Duration
	for _, w := range *waits {
		total += w
		if w > time.Minute {
			t.Fatalf("a wait of %v exceeds the 1m cap", w)
		}
	}
	if total > 15*time.Minute || total < 14*time.Minute {
		t.Fatalf("waited %v in %d calls, want just under the 15m window", total, calls)
	}
}

func TestDownloadRetryProgressResetsWindowAndBackoff(t *testing.T) {
	p, waits, pass := fakeRetryClock()
	calls := 0
	err := p.run(context.Background(), func(context.Context, int) (bool, error) {
		calls++
		if calls == 40 {
			return true, nil
		}
		// Every attempt moves bytes for a minute before it drops: 39 minutes
		// in all, which ends a window that is not reset by progress.
		pass(time.Minute)
		return true, status.Error(codes.Unavailable, "flaky")
	}, func(int, error) {})
	if err != nil {
		t.Fatalf("a link that keeps making progress was given up on: %v", err)
	}
	for _, w := range *waits {
		if w != 2*time.Second {
			t.Fatalf("backoff grew to %v although every attempt progressed", w)
		}
	}
}

func TestDownloadRetryStopsOnRefusalAndCancel(t *testing.T) {
	p, _ := fakeRetry()
	calls := 0
	refused := status.Error(codes.NotFound, "file not found")
	err := p.run(context.Background(), func(context.Context, int) (bool, error) {
		calls++
		return false, refused
	}, func(int, error) {})
	if !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want the refusal after one call", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	err = p.run(ctx, func(context.Context, int) (bool, error) {
		cancel()
		return false, status.Error(codes.Canceled, "context canceled")
	}, func(int, error) { t.Fatal("a cancelled download must not retry") })
	if !errors.Is(err, errDownloadCancelled) {
		t.Fatalf("err = %v, want errDownloadCancelled", err)
	}
}
