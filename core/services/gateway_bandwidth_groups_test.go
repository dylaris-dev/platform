package services

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Each Core reads through its own group, named after its CoreID - the container
// hostname when DYLARIS_CORE_ID is unset, so every redeploy brings a new one.
// A group whose every consumer has been idle past gwbwGroupIdleAfter belongs to
// a Core that is gone and is destroyed; a live group, this Core's own, one
// nobody has read from yet and any other service's group are kept.
func TestDropStaleGroupsDestroysOnlyDeadCoresGroups(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	const key = "dylaris:edge:e1:stats"
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mr.SetTime(t0)
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"data": "{}"}}).Err(); err != nil {
		t.Fatal(err)
	}

	read := func(group, consumer string) {
		t.Helper()
		if err := rdb.XGroupCreateMkStream(ctx, key, group, "0").Err(); err != nil {
			t.Fatal(err)
		}
		res, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: group, Consumer: consumer, Streams: []string{key, ">"}, Count: 1, Block: -1}).Result()
		if err != nil {
			t.Fatal(err)
		}
		// miniredis records a consumer's idle time on XCLAIM only.
		if err := rdb.XClaim(ctx, &redis.XClaimArgs{Stream: key, Group: group, Consumer: consumer, Messages: []string{res[0].Messages[0].ID}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	read(gwbwGroup+"-dead", "dead")
	read(gwbwGroup+"-core-a", "core-a")
	read("dylaris-core-stats", "dead") // another service's group
	mr.SetTime(t0.Add(gwbwGroupIdleAfter + time.Hour))
	read(gwbwGroup+"-live", "live")
	if err := rdb.XGroupCreateMkStream(ctx, key, gwbwGroup+"-fresh", "0").Err(); err != nil {
		t.Fatal(err)
	}
	// A quiet stream: the idle group has read everything there is, so its
	// idle time says nothing and it stays.
	const quiet = "dylaris:link:l1:stats"
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: quiet, Values: map[string]any{"data": "{}"}}).Err(); err != nil {
		t.Fatal(err)
	}
	mr.SetTime(t0)
	if err := rdb.XGroupCreateMkStream(ctx, quiet, gwbwGroup+"-dead", "0").Err(); err != nil {
		t.Fatal(err)
	}
	res, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: gwbwGroup + "-dead", Consumer: "dead", Streams: []string{quiet, ">"}, Count: 1, Block: -1}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.XClaim(ctx, &redis.XClaimArgs{Stream: quiet, Group: gwbwGroup + "-dead", Consumer: "dead", Messages: []string{res[0].Messages[0].ID}}).Err(); err != nil {
		t.Fatal(err)
	}
	mr.SetTime(t0.Add(gwbwGroupIdleAfter + time.Hour))
	// The busy stream moved on past what the dead Core read.
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"data": "{}"}}).Err(); err != nil {
		t.Fatal(err)
	}

	s := NewGatewayBandwidthConsumerService(nil, rdb, "core-a")
	s.dropStaleGroups(ctx, key)
	s.dropStaleGroups(ctx, quiet)
	if qg, err := rdb.XInfoGroups(ctx, quiet).Result(); err != nil || len(qg) != 1 {
		t.Errorf("quiet stream groups = %v (%v), want the idle group kept", qg, err)
	}

	groups, err := rdb.XInfoGroups(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, g := range groups {
		have[g.Name] = true
	}
	want := map[string]bool{
		gwbwGroup + "-dead":   false,
		gwbwGroup + "-core-a": true,
		gwbwGroup + "-live":   true,
		gwbwGroup + "-fresh":  true,
		"dylaris-core-stats":  true,
	}
	for name, keep := range want {
		if have[name] != keep {
			t.Errorf("group %s present = %v, want %v", name, have[name], keep)
		}
	}
}
