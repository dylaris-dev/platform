package main

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A transient Redis fault must not hand the tenant an uncapped transfer.
//
// The three Get errors used to be swallowed, so an unreachable Redis produced
// 0, 0, 0 and setLimits(0, 0) installed nil limiters - unlimited - until the
// next successful 10s poll. The relay's copy of this function has guarded it
// all along, with the reason in a comment; the node's did not. One idea, two
// implementations, one of them took the guard.
//
// An UNSET key is a different thing and still means no cap, which the second
// half of this pins: fixing the fault case by refusing every empty read would
// have made "no limit configured" unreachable.
func TestBeamThrottleKeepsItsLimitsWhenRedisIsDown(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	ctx := context.Background()

	bt := &BeamThrottle{}
	mr.Set("beam:bw_up_internal", "1048576")
	mr.Set("beam:bw_down_internal", "2097152")
	bt.reloadFromRedis(ctx, rdb)
	if bt.upLimiter == nil || bt.downLimiter == nil {
		t.Fatal("the configured caps were not installed")
	}
	up, down := bt.upLimiter, bt.downLimiter

	// Redis goes away. The caps must survive it.
	mr.Close()
	bt.reloadFromRedis(ctx, rdb)
	if bt.upLimiter == nil || bt.downLimiter == nil {
		t.Fatal("an unreachable Redis dropped the cap to unlimited")
	}
	if bt.upLimiter != up || bt.downLimiter != down {
		t.Error("the limiters were rebuilt from a failed read instead of kept")
	}
}

// The other half: no keys at all is a real answer, and it means no cap.
func TestBeamThrottleUnsetKeysStillMeanNoCap(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	bt := &BeamThrottle{}
	bt.reloadFromRedis(context.Background(), rdb)
	if bt.upLimiter != nil || bt.downLimiter != nil {
		t.Error("an installation with no throttle configured got one anyway")
	}
}
