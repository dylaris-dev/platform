package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// The disk gauge (dylaris:server:<uuid>:stats:disk) is a MEASUREMENT, five
// minutes apart on a node without project quotas. Every upload check - Core's,
// Beam's and SFTP's - reads it, so a file closed after one measurement was
// invisible to the next check: upload, close, upload again, and each saw the
// whole headroom once more, as often as fit in the five minutes.
//
// What the node writes for a tenant in between is therefore added to the
// published gauge at once, and kept until a measurement that STARTED after the
// write replaces it.

type unmeasuredWrite struct {
	at time.Time
	n  int64
}

// unmeasured holds per server the writes no published measurement has seen.
// One lock for the node: it also orders the gauge's read-modify-write against
// a new measurement being published.
// last is when the published measurement began: one that began earlier and
// finished later - a long du against a quick remeasure - must not replace it.
var unmeasured = struct {
	sync.Mutex
	m    map[string][]unmeasuredWrite
	last map[string]time.Time
}{m: map[string][]unmeasuredWrite{}, last: map[string]time.Time{}}

// notedBytes counts, per server, every byte noteDiskWrite added, and only
// grows (uuid -> *atomic.Int64). An SFTP handle takes its ceiling from the
// gauge once, at open; what was noted since is what that ceiling lacks.
var notedBytes sync.Map

func notedFor(uuid string) *atomic.Int64 {
	c, _ := notedBytes.LoadOrStore(uuid, new(atomic.Int64))
	return c.(*atomic.Int64)
}

// unmeasuredKeep outlives every gauge TTL: past it there is no gauge left to
// adjust, and the next one is a fresh measurement.
const unmeasuredKeep = stoppedDiskUsageTTL

func diskGaugeKey(uuid string) string { return fmt.Sprintf("dylaris:server:%s:stats:disk", uuid) }

// sinceWrites drops the writes before cutoff.
func sinceWrites(ws []unmeasuredWrite, cutoff time.Time) []unmeasuredWrite {
	i := 0
	for i < len(ws) && ws[i].at.Before(cutoff) {
		i++
	}
	return ws[i:]
}

// noteDiskWrite adds n bytes the server just grew by to its published gauge.
// Without a gauge there is nothing to correct: the next measurement sees them.
// Bounded in time: the mesh calls it on the loop that reads every Core message.
func noteDiskWrite(ctx context.Context, rdb *redis.Client, uuid string, n int64) {
	if rdb == nil || uuid == "" || n <= 0 {
		return
	}
	notedFor(uuid).Add(n)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	unmeasured.Lock()
	defer unmeasured.Unlock()
	now := time.Now()
	unmeasured.m[uuid] = append(sinceWrites(unmeasured.m[uuid], now.Add(-unmeasuredKeep)), unmeasuredWrite{now, n})

	raw, err := rdb.Get(ctx, diskGaugeKey(uuid)).Result()
	if err != nil {
		return
	}
	// Read rather than kept: a key that expired in between would come back
	// with no expiry at all.
	ttl, err := rdb.PTTL(ctx, diskGaugeKey(uuid)).Result()
	if err != nil || ttl <= 0 {
		return
	}
	var u DiskUsagePayload
	if json.Unmarshal([]byte(raw), &u) != nil {
		return
	}
	u.Total += n
	data, _ := json.Marshal(u)
	rdb.Set(ctx, diskGaugeKey(uuid), string(data), ttl)
}

// publishDiskUsage stores a measurement that began at measuredFrom, plus the
// writes it cannot have seen. A write during the measurement may be in it as
// well; it stays counted, the safe side, until the next one.
func publishDiskUsage(ctx context.Context, rdb *redis.Client, uuid string, usage *DiskUsagePayload, measuredFrom time.Time, ttl time.Duration) {
	unmeasured.Lock()
	defer unmeasured.Unlock()
	ws := sinceWrites(unmeasured.m[uuid], measuredFrom)
	if len(ws) == 0 {
		delete(unmeasured.m, uuid)
	} else {
		unmeasured.m[uuid] = ws
	}
	if measuredFrom.Before(unmeasured.last[uuid]) {
		return
	}
	unmeasured.last[uuid] = measuredFrom
	pub := *usage
	for _, w := range ws {
		pub.Total += w.n
	}
	data, _ := json.Marshal(pub)
	rdb.Set(ctx, diskGaugeKey(uuid), string(data), ttl)
}

// remeasureServer publishes a fresh measurement after a copy, extraction or
// install. Those write and remove - a wipe before an install, temporary
// archives, a failed half - so counting their bytes left the gauge reading a
// server full that was not, and refused every upload until the next sweep.
func remeasureServer(uuid string) {
	sm := globalStorageMgr
	if sm == nil || sm.rdb == nil || uuid == "" {
		return
	}
	ctx := context.Background()
	from := time.Now()
	if usage := getDiskUsage(ctx, sm.rdb, uuid, globalQuotaSet); usage != nil {
		publishDiskUsage(ctx, sm.rdb, uuid, usage, from, 10*time.Minute)
	}
}
