package auth

import (
	"testing"
	"time"
)

// TestAPIKeyRateLimiterAllow mirrors the IPRateLimiter.allow test style
// (ratelimit_test.go) for the api-key sliding-window limiter: burst up to
// perMin passes, the next request is denied, keys are isolated from each
// other, and a bucket manually pushed into the past resets on next allow().
func TestAPIKeyRateLimiterAllow(t *testing.T) {
	t.Run("burst up to perMin passes then next denied", func(t *testing.T) {
		l := newAPIKeyRateLimiter()
		const perMin = 3
		for i := 1; i <= perMin; i++ {
			if !l.allow(1, perMin) {
				t.Fatalf("request %d expected allowed, got denied", i)
			}
		}
		if l.allow(1, perMin) {
			t.Fatalf("request %d (perMin+1) expected denied, got allowed", perMin+1)
		}
	})

	t.Run("per-key isolation", func(t *testing.T) {
		l := newAPIKeyRateLimiter()
		const perMin = 2
		for i := 1; i <= perMin; i++ {
			if !l.allow(10, perMin) {
				t.Fatalf("key10 request %d expected allowed", i)
			}
		}
		if l.allow(10, perMin) {
			t.Fatalf("key10 request %d expected denied (budget exhausted)", perMin+1)
		}
		for i := 1; i <= perMin; i++ {
			if !l.allow(20, perMin) {
				t.Fatalf("key20 request %d expected allowed, key10's exhaustion leaked", i)
			}
		}
		if l.allow(20, perMin) {
			t.Fatalf("key20 request %d expected denied", perMin+1)
		}
	})

	t.Run("perMin<=0 falls back to default budget of 60", func(t *testing.T) {
		l := newAPIKeyRateLimiter()
		for i := 1; i <= 60; i++ {
			if !l.allow(30, 0) {
				t.Fatalf("request %d expected allowed under default 60/min budget", i)
			}
		}
		if l.allow(30, 0) {
			t.Fatalf("61st request expected denied under default 60/min budget")
		}
		if !l.allow(31, -5) {
			t.Fatalf("first request with negative perMin expected allowed (falls back to default)")
		}
	})

	t.Run("window reset after expiry allows again", func(t *testing.T) {
		// allow() has no injected clock, only time.Now(); rewrite the
		// unexported bucket's reset time into the past instead of sleeping
		// a full minute in a unit test (same technique as ratelimit_test.go).
		l := newAPIKeyRateLimiter()
		const perMin = 1
		if !l.allow(40, perMin) {
			t.Fatalf("first request expected allowed")
		}
		if l.allow(40, perMin) {
			t.Fatalf("second request expected denied within the same window")
		}
		l.mu.Lock()
		l.buckets[40].reset = time.Now().Add(-time.Second)
		l.mu.Unlock()
		if !l.allow(40, perMin) {
			t.Fatalf("request after simulated window elapse expected allowed (bucket should reset)")
		}
		if l.allow(40, perMin) {
			t.Fatalf("second request in the new window expected denied")
		}
	})
}

// TestAPIKeyRateLimiterShouldTouch pins the once-per-minute-per-key gate on
// the last-used DB stamp.
func TestAPIKeyRateLimiterShouldTouch(t *testing.T) {
	l := newAPIKeyRateLimiter()

	if !l.shouldTouch(1) {
		t.Fatalf("first shouldTouch(1) expected true")
	}
	if l.shouldTouch(1) {
		t.Fatalf("second shouldTouch(1) within the same minute expected false")
	}
	// A different key is not gated by key 1's touch.
	if !l.shouldTouch(2) {
		t.Fatalf("shouldTouch(2) expected true (independent from key 1)")
	}

	// Simulate more than a minute having passed for key 1.
	l.mu.Lock()
	l.lastTouch[1] = time.Now().Add(-2 * time.Minute)
	l.mu.Unlock()
	if !l.shouldTouch(1) {
		t.Fatalf("shouldTouch(1) after simulated minute elapse expected true")
	}
}
