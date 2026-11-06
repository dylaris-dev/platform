package main

import (
	"testing"
	"time"
)

// resetPublicIPCache clears the cache and installs a stub lookup for one test.
func resetPublicIPCache(t *testing.T, lookup func() string) *int {
	t.Helper()
	calls := 0
	prev := publicIPLookup
	publicIPLookup = func() string {
		calls++
		return lookup()
	}
	publicIPMu.Lock()
	publicIPVal, publicIPAt, publicIPFailAt, publicIPWarn = "", time.Time{}, time.Time{}, false
	publicIPMu.Unlock()
	t.Cleanup(func() {
		publicIPLookup = prev
		publicIPMu.Lock()
		publicIPVal, publicIPAt, publicIPFailAt, publicIPWarn = "", time.Time{}, time.Time{}, false
		publicIPMu.Unlock()
	})
	return &calls
}

// The heartbeat ticks every 5s and calls this on every beat. Unthrottled that
// is three external requests per node per 5 seconds against a free service.
func TestPublicIPIsCachedBetweenHeartbeats(t *testing.T) {
	calls := resetPublicIPCache(t, func() string { return "203.0.113.7" })

	for range 20 {
		if got := getOutboundIP(); got != "203.0.113.7" {
			t.Fatalf("got %q, want 203.0.113.7", got)
		}
	}
	if *calls != 1 {
		t.Fatalf("the lookup ran %d times for 20 heartbeats; it should be cached for %s", *calls, publicIPTTL)
	}
}

// A failed refresh must not fall through to the UDP routing trick. Core
// PERSISTS whatever the heartbeat reports (services/discovery.go
// SetNodeAddress), and inside Swarm that trick answers with the overlay
// address - so an outage would replace a correct public address with an
// unroutable one, and it would stay wrong after the outage ended.
func TestPublicIPKeepsTheLastKnownAddressWhenTheLookupFails(t *testing.T) {
	fail := false
	resetPublicIPCache(t, func() string {
		if fail {
			return ""
		}
		return "203.0.113.7"
	})

	if got := getOutboundIP(); got != "203.0.113.7" {
		t.Fatalf("first lookup: got %q", got)
	}

	// Expire the cache and make every service unreachable.
	publicIPMu.Lock()
	publicIPAt = time.Now().Add(-2 * publicIPTTL)
	publicIPMu.Unlock()
	fail = true

	if got := getOutboundIP(); got != "203.0.113.7" {
		t.Fatalf("a failed refresh returned %q instead of keeping the last known address; "+
			"Core persists this value as the node's public address", got)
	}
}

// The refresh must actually happen once the entry is stale, or a machine whose
// address changed would advertise the old one forever.
func TestPublicIPRefreshesAfterTheTTL(t *testing.T) {
	current := "203.0.113.7"
	calls := resetPublicIPCache(t, func() string { return current })

	getOutboundIP()
	publicIPMu.Lock()
	publicIPAt = time.Now().Add(-2 * publicIPTTL)
	publicIPMu.Unlock()
	current = "198.51.100.9"

	if got := getOutboundIP(); got != "198.51.100.9" {
		t.Fatalf("got %q, want the refreshed 198.51.100.9", got)
	}
	if *calls != 2 {
		t.Fatalf("the lookup ran %d times, want 2 (one initial, one after the TTL)", *calls)
	}
}

// M03: a failed refresh must wait like a successful one, or every 5s beat
// runs all three lookups (up to 9s when they are blackholed).
func TestPublicIPFailureIsThrottled(t *testing.T) {
	fail := false
	calls := resetPublicIPCache(t, func() string {
		if fail {
			return ""
		}
		return "203.0.113.7"
	})
	getOutboundIP()
	publicIPMu.Lock()
	publicIPAt = time.Now().Add(-2 * publicIPTTL)
	publicIPMu.Unlock()
	fail = true
	for range 10 {
		if got := getOutboundIP(); got != "203.0.113.7" {
			t.Fatalf("got %q", got)
		}
	}
	if *calls != 2 {
		t.Fatalf("the lookup ran %d times, want 2: a failure must not be retried on every beat", *calls)
	}
}
