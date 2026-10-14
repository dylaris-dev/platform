package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A machine whose clock is minutes off still stamps heartbeats Core accepts:
// the stamp follows Redis's clock, measured again only now and then.
func TestRedisClockCorrectsADriftedMachine(t *testing.T) {
	calls := 0
	ahead := 7 * time.Minute
	c := &redisClock{redis: func(context.Context) (time.Time, error) {
		calls++
		return time.Now().Add(ahead), nil
	}}
	got := c.now(context.Background())
	if d := got.Sub(time.Now().Add(ahead)).Abs(); d > time.Second {
		t.Fatalf("corrected time is %s off Redis's", d)
	}
	c.now(context.Background())
	if calls != 1 {
		t.Fatalf("Redis asked %d times within the refresh interval", calls)
	}

	// A failed measurement keeps the last offset rather than falling back.
	c.at = time.Now().Add(-2 * redisClockRefresh)
	c.redis = func(context.Context) (time.Time, error) { return time.Time{}, errors.New("redis down") }
	if d := c.now(context.Background()).Sub(time.Now().Add(ahead)).Abs(); d > time.Second {
		t.Fatalf("a failed measurement lost the offset: %s off", d)
	}
}

// A clock within the tolerance is trusted: a node whose clock is right does
// not depend on the Redis host's.
func TestRedisClockTrustsASyncedMachine(t *testing.T) {
	c := &redisClock{redis: func(context.Context) (time.Time, error) {
		return time.Now().Add(3 * time.Second), nil
	}}
	if d := c.now(context.Background()).Sub(time.Now()).Abs(); d > 500*time.Millisecond {
		t.Fatalf("a 3s difference moved the clock by %s", d)
	}
}

// Redis's time is read off a stream id, since the node's ACL refuses TIME.
func TestStreamIDTime(t *testing.T) {
	got, err := streamIDTime("1760000000123-4")
	if err != nil || !got.Equal(time.UnixMilli(1760000000123)) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := streamIDTime("nonsense"); err == nil {
		t.Fatal("a malformed id parsed")
	}
}
