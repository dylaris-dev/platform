package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// BeamThrottle manages per-direction bandwidth limiting for all Beam file
// transfer streams on this node. Two independent rate.Limiters back the
// upload (bytes flowing from client → node, written to disk) and download
// (bytes flowing from disk → client) paths so a hoster can cap them
// asymmetrically — datacenter uplinks often advertise different up/down
// speeds, and a Beam transfer should respect that.
//
// Each limiter is a shared instance across all streams in its direction,
// which gives automatic max-min fair sharing:
//   - 2 simultaneous uploads → each gets ~half the upload cap
//   - 1 fast + 1 slow user → fast user gets the remainder automatically
//   - Only 1 stream → gets the full cap
//
// Setting a cap to 0 removes the limiter (unlimited).
type BeamThrottle struct {
	mu sync.RWMutex

	upLimiter   *rate.Limiter // chunks written to disk on upload
	upLimit     int64
	downLimiter *rate.Limiter // chunks read from disk on download
	downLimit   int64
}

// Direction selects which limiter WaitN applies.
type Direction int

const (
	DirectionUp   Direction = iota // upload (client → disk)
	DirectionDown                  // download (disk → client)
)

// NewBeamThrottle creates a new throttle. It starts a background goroutine
// that watches Redis for bandwidth limit changes from Core settings.
//
// Reads three Redis keys (kept by Core's SaveBeamSettings handler):
//   - beam:bw_up_internal   — upload cap, bytes/sec
//   - beam:bw_down_internal — download cap, bytes/sec
//   - beam:bw_limit         — legacy symmetric cap (back-compat)
//
// When the per-direction keys are unset, both directions fall back to
// the legacy symmetric beam:bw_limit so existing deploys keep working.
func NewBeamThrottle(ctx context.Context, rdb *redis.Client) *BeamThrottle {
	bt := &BeamThrottle{}

	if rdb != nil {
		bt.reloadFromRedis(ctx, rdb)
		go bt.watchLimits(ctx, rdb)
	}

	return bt
}

// WaitN blocks until n bytes are allowed in the given direction, or ctx
// is cancelled. If the direction is unlimited (limit=0), returns
// immediately. Choosing the direction matters: an asymmetric cap (say
// 1 Gbit/s down, 100 Mbit/s up) only enforces correctly when each call
// site selects the right one.
func (bt *BeamThrottle) WaitN(ctx context.Context, dir Direction, n int) error {
	bt.mu.RLock()
	var lim *rate.Limiter
	switch dir {
	case DirectionUp:
		lim = bt.upLimiter
	case DirectionDown:
		lim = bt.downLimiter
	}
	bt.mu.RUnlock()

	if lim == nil {
		return nil // unlimited
	}
	return lim.WaitN(ctx, n)
}

// UpLimit / DownLimit expose the current caps for diagnostics.
func (bt *BeamThrottle) UpLimit() int64 {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	return bt.upLimit
}

func (bt *BeamThrottle) DownLimit() int64 {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	return bt.downLimit
}

// setLimits writes both directions in one critical section so a watcher
// update flips them together. A 0 value removes that direction's limiter
// (unlimited).
func (bt *BeamThrottle) setLimits(up, down int64) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	if up != bt.upLimit {
		bt.upLimiter = makeLimiter(up)
		bt.upLimit = up
		logLimit("up", up)
	}
	if down != bt.downLimit {
		bt.downLimiter = makeLimiter(down)
		bt.downLimit = down
		logLimit("down", down)
	}
}

func makeLimiter(bytesPerSec int64) *rate.Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	// Burst = up to 1s worth, capped at 256KB so a packed burst can't
	// dwarf the running rate. Floor at 64KB so we don't stall tiny caps.
	burst := int(bytesPerSec)
	if burst > 256*1024 {
		burst = 256 * 1024
	}
	if burst < 64*1024 {
		burst = 64 * 1024
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

func logLimit(dir string, bytesPerSec int64) {
	if bytesPerSec <= 0 {
		log.Printf("beam-throttle: %s unlimited", dir)
		return
	}
	log.Printf("beam-throttle: %s capped at %d bytes/sec (%.1f MB/s)", dir, bytesPerSec, float64(bytesPerSec)/(1024*1024))
}

// failed separates "the key is not set" from "Redis did not answer". Only the
// second one must stop a reload.
func failed(err error) bool {
	return err != nil && !errors.Is(err, redis.Nil)
}

// reloadFromRedis reads the current keys and applies them. The order
// matters: per-direction keys win over the legacy symmetric key.
func (bt *BeamThrottle) reloadFromRedis(ctx context.Context, rdb *redis.Client) {
	legacy, legacyErr := rdb.Get(ctx, "beam:bw_limit").Int64()
	up, upErr := rdb.Get(ctx, "beam:bw_up_internal").Int64()
	down, downErr := rdb.Get(ctx, "beam:bw_down_internal").Int64()

	// redis.Nil just means the key is unset, which legitimately means no cap.
	// Any OTHER error is Redis being unreachable, and reading that as "no cap"
	// hands the tenant an uncapped transfer until the next poll succeeds. The
	// relay's copy of this function guards it; this one swallowed all three
	// errors and called setLimits(0, 0).
	if failed(legacyErr) || failed(upErr) || failed(downErr) {
		log.Printf("beam-throttle: Redis reload failed, keeping current limits (legacy=%v up=%v down=%v)",
			legacyErr, upErr, downErr)
		return
	}

	if up == 0 {
		up = legacy
	}
	if down == 0 {
		down = legacy
	}
	bt.setLimits(up, down)
}

// watchLimits polls Redis every 10 seconds for any cap change.
func (bt *BeamThrottle) watchLimits(ctx context.Context, rdb *redis.Client) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bt.reloadFromRedis(ctx, rdb)
		}
	}
}

// ServerDataThrottle paces one server's beam bytes while Core has throttled
// its tenant's data traffic (over the allowance without data overage). Core
// publishes dylaris:server:<uuid>:data_bw in bytes/s; a missing key is no
// throttle. One limiter per server, shared by all of its streams, on top of
// the node-wide BeamThrottle. Read through a 10 s cache: the key changes at
// most once a minute.
type ServerDataThrottle struct {
	rdb *redis.Client
	mu  sync.Mutex
	m   map[string]*serverDataLimit
}

type serverDataLimit struct {
	bps int64
	at  time.Time
	lim *rate.Limiter
}

func NewServerDataThrottle(rdb *redis.Client) *ServerDataThrottle {
	return &ServerDataThrottle{rdb: rdb, m: map[string]*serverDataLimit{}}
}

func (t *ServerDataThrottle) limiter(ctx context.Context, uuid string) *rate.Limiter {
	t.mu.Lock()
	e := t.m[uuid]
	fresh := e != nil && time.Since(e.at) < 10*time.Second
	t.mu.Unlock()
	if fresh {
		return e.lim
	}
	var bps int64
	if t.rdb != nil {
		v, err := t.rdb.Get(ctx, "dylaris:server:"+uuid+":data_bw").Int64()
		if err == nil && v > 0 {
			bps = v
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e = t.m[uuid]
	if e == nil || e.bps != bps {
		e = &serverDataLimit{bps: bps}
		if bps > 0 {
			// Burst of one second's worth: a 32 KB chunk always fits.
			e.lim = rate.NewLimiter(rate.Limit(bps), int(bps))
		}
		t.m[uuid] = e
	}
	e.at = time.Now()
	return e.lim
}

// WaitN blocks until n bytes of the server's data may pass.
func (t *ServerDataThrottle) WaitN(ctx context.Context, uuid string, n int) error {
	if t == nil || uuid == "" || n <= 0 {
		return nil
	}
	lim := t.limiter(ctx, uuid)
	if lim == nil {
		return nil
	}
	for n > 0 {
		step := n
		if b := lim.Burst(); step > b {
			step = b
		}
		if err := lim.WaitN(ctx, step); err != nil {
			return err
		}
		n -= step
	}
	return nil
}
