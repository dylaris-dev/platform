package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// redisClockRefresh is how often the offset to Redis's clock is measured:
	// often, so an NTP step or a resumed VM is followed within a heartbeat or so.
	redisClockRefresh = 30 * time.Second
	// redisClockTolerance is the drift the node's own clock is trusted within.
	// Only a clock further off than this follows Redis's, so a node whose clock
	// is right never depends on the Redis host's.
	redisClockTolerance = 10 * time.Second
)

// redisClock is the node's clock, corrected to Redis's when it has drifted.
// Core accepts a signed heartbeat only within 30 seconds of its own clock, so a
// machine without NTP, or a VM after a suspend, drifted past that and showed as
// offline for good while everything else worked - and nothing said why. Core
// and Redis run on the platform's own synced hosts; the customer's machine is
// the one that drifts.
type redisClock struct {
	mu     sync.Mutex
	off    time.Duration
	at     time.Time
	warned bool
	redis  func(context.Context) (time.Time, error)
}

// now returns the corrected time, measuring the offset again when it is old.
// A failed measurement keeps the last offset.
func (c *redisClock) now(ctx context.Context) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) > redisClockRefresh {
		before := time.Now()
		if rt, err := c.redis(ctx); err == nil {
			after := time.Now()
			// Against the middle of the round trip.
			off := rt.Sub(before.Add(after.Sub(before) / 2))
			c.off = 0
			if off.Abs() > redisClockTolerance {
				c.off = off
				if !c.warned {
					c.warned = true
					log.Printf("heartbeat: this machine's clock is %s off; using Redis's time - set up NTP on the host", off.Round(time.Second))
				}
			}
			c.at = after
		}
	}
	return time.Now().Add(c.off)
}

var (
	hbClockOnce sync.Once
	hbClock     *redisClock
)

// heartbeatClock is the clock heartbeats and command ages are judged by.
//
// Redis's time is read off the id of an entry the node adds to a stream of
// its own: an auto id is the Redis server's clock in milliseconds. TIME would
// be the obvious command, and the node's ACL refuses it.
func heartbeatClock(rdb *redis.Client, id string) *redisClock {
	hbClockOnce.Do(func() {
		hbClock = &redisClock{redis: func(ctx context.Context) (time.Time, error) {
			tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			key := fmt.Sprintf("dylaris:node:%s:clock", id)
			pipe := rdb.Pipeline()
			add := pipe.XAdd(tctx, &redis.XAddArgs{Stream: key, MaxLen: 1, Values: map[string]any{"t": "1"}})
			// Gone with the node, not left behind for good.
			pipe.Expire(tctx, key, time.Hour)
			if _, err := pipe.Exec(tctx); err != nil {
				return time.Time{}, err
			}
			return streamIDTime(add.Val())
		}}
	})
	return hbClock
}

// streamIDTime reads the server time out of a Redis stream id ("<ms>-<seq>").
func streamIDTime(id string) (time.Time, error) {
	ms, err := strconv.ParseInt(strings.SplitN(id, "-", 2)[0], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("stream id %q: %w", id, err)
	}
	return time.UnixMilli(ms), nil
}
