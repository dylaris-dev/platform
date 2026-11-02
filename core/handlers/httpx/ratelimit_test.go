package httpx

import (
	"fmt"
	"testing"
	"time"
)

func TestIPRateLimiterAllow(t *testing.T) {
	t.Run("within limit allowed then N+1th denied", func(t *testing.T) {
		l := NewIPRateLimiter()
		const perMin = 3
		for i := 1; i <= perMin; i++ {
			if !l.Allow("1.2.3.4", perMin) {
				t.Fatalf("request %d expected allowed, got denied", i)
			}
		}
		if l.Allow("1.2.3.4", perMin) {
			t.Fatalf("request %d (N+1th) expected denied, got allowed", perMin+1)
		}
		// Still denied on a subsequent attempt within the same window.
		if l.Allow("1.2.3.4", perMin) {
			t.Fatalf("request %d expected denied, got allowed", perMin+2)
		}
	})

	t.Run("per-IP isolation", func(t *testing.T) {
		l := NewIPRateLimiter()
		const perMin = 2
		for i := 1; i <= perMin; i++ {
			if !l.Allow("10.0.0.1", perMin) {
				t.Fatalf("ip1 request %d expected allowed", i)
			}
		}
		if l.Allow("10.0.0.1", perMin) {
			t.Fatalf("ip1 request %d expected denied (budget exhausted)", perMin+1)
		}
		// A second IP must have its own independent budget.
		for i := 1; i <= perMin; i++ {
			if !l.Allow("10.0.0.2", perMin) {
				t.Fatalf("ip2 request %d expected allowed, ip1's exhaustion leaked", i)
			}
		}
		if l.Allow("10.0.0.2", perMin) {
			t.Fatalf("ip2 request %d expected denied", perMin+1)
		}
	})

	t.Run("perMin<=0 falls back to default budget of 60", func(t *testing.T) {
		l := NewIPRateLimiter()
		for i := 1; i <= 60; i++ {
			if !l.Allow("5.5.5.5", 0) {
				t.Fatalf("request %d expected allowed under default 60/min budget", i)
			}
		}
		if l.Allow("5.5.5.5", 0) {
			t.Fatalf("61st request expected denied under default 60/min budget")
		}
		// Negative values are treated the same as zero.
		if !l.Allow("6.6.6.6", -5) {
			t.Fatalf("first request with negative perMin expected allowed (falls back to default)")
		}
	})

	t.Run("window reset after expiry allows again", func(t *testing.T) {
		// allow() has no injected clock, only time.Now(); sleeping a full
		// minute in a unit test is impractical, so this rewrites the
		// unexported bucket's reset time into the past (equivalent to what
		// real elapsed time would produce) instead of adding a clock seam to
		// production code.
		l := NewIPRateLimiter()
		const perMin = 1
		if !l.Allow("7.7.7.7", perMin) {
			t.Fatalf("first request expected allowed")
		}
		if l.Allow("7.7.7.7", perMin) {
			t.Fatalf("second request expected denied within the same window")
		}
		l.mu.Lock()
		l.buckets["7.7.7.7"].reset = time.Now().Add(-time.Second)
		l.mu.Unlock()
		if !l.Allow("7.7.7.7", perMin) {
			t.Fatalf("request after simulated window elapse expected allowed (bucket should reset)")
		}
		// The freshly-reset bucket should itself enforce the limit again.
		if l.Allow("7.7.7.7", perMin) {
			t.Fatalf("second request in the new window expected denied")
		}
	})

	t.Run("bucket map eviction sweeps expired entries once size exceeds 10000", func(t *testing.T) {
		l := NewIPRateLimiter()
		l.mu.Lock()
		for i := 0; i < 10001; i++ {
			l.buckets[fmt.Sprintf("ip-%d", i)] = &rateBucket{count: 1, reset: time.Now().Add(-time.Minute)}
		}
		l.mu.Unlock()

		if !l.Allow("new-ip", 5) {
			t.Fatalf("allow on a fresh IP expected true")
		}

		l.mu.Lock()
		n := len(l.buckets)
		l.mu.Unlock()
		// Every pre-seeded bucket was already expired, so the opportunistic
		// sweep should have deleted all of them, leaving only the bucket
		// this allow() call just created.
		if n != 1 {
			t.Errorf("expected 1 bucket left after the eviction sweep, got %d", n)
		}
	})
}

// TestIPRateLimiter_BoundedUnderLiveIPFlood covers the case the expired-only
// sweep missed and the old doc comment wrongly claimed to handle: a flood of
// DISTINCT, still-LIVE IPs. Nothing is expired, so without the arbitrary
// eviction the map grows without limit. Feeding well past the ceiling must
// leave it bounded.
func TestIPRateLimiter_BoundedUnderLiveIPFlood(t *testing.T) {
	l := NewIPRateLimiter()

	const feed = maxBuckets * 3
	for i := 0; i < feed; i++ {
		// Every key distinct and fresh: no bucket is ever expired during the
		// loop, so the expired sweep alone would never bring the map down.
		l.Allow(fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256), 60)
	}

	l.mu.Lock()
	size := len(l.buckets)
	l.mu.Unlock()

	if size > maxBuckets {
		t.Fatalf("map holds %d buckets after %d distinct live IPs, want <= %d", size, feed, maxBuckets)
	}
}

// TestIPRateLimiter_BudgetSurvivesEviction is the guard the bound must not
// break: the same IP is still counted to its limit and then denied.
func TestIPRateLimiter_BudgetSurvivesEviction(t *testing.T) {
	l := NewIPRateLimiter()
	const limit = 5

	allowed := 0
	for i := 0; i < limit+3; i++ {
		if l.Allow("203.0.113.7", limit) {
			allowed++
		}
	}
	if allowed != limit {
		t.Fatalf("allowed %d requests, want exactly the %d-request budget", allowed, limit)
	}
}
