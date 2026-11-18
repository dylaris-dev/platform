package services

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The infrastructure half of the record: what Core's own process costs, what
// Postgres and Redis are doing underneath it, and how big the data has grown.
//
// All of it comes from what the two databases already publish about themselves.
// Nothing here instruments a query path, and nothing added a dependency: the
// process figures are read from /proc, which is what Core runs on.

// counterSource remembers the previous reading of a CUMULATIVE counter so the
// recorded value can be the delta.
//
// Postgres and Redis both report totals since their own start. Recording those
// directly would produce a line that only ever goes up and whose slope is the
// actual information, and it would fall off a cliff on every restart. The delta
// is what merges correctly into a bucket, and a NEGATIVE delta identifies a
// restart and is discarded rather than recorded as a huge value.
type counterSource struct{ last map[string]float64 }

func newCounterSource() *counterSource { return &counterSource{last: map[string]float64{}} }

// delta returns the increase since the previous reading and whether there is
// one to record. The first reading of any counter establishes a baseline and
// records nothing - a first sample of a total would otherwise be recorded as if
// the whole of it happened in that one window.
func (c *counterSource) delta(name string, cur float64) (float64, bool) {
	prev, seen := c.last[name]
	c.last[name] = cur
	if !seen || cur < prev {
		return 0, false
	}
	return cur - prev, true
}

// sampleSelf records what the Core PROCESS costs.
//
// Read from /proc rather than through a system-metrics library, because Core
// had no such dependency and one process's own resident size and CPU time are
// two small file reads. On a platform without /proc the reads fail and nothing
// is recorded, which is the honest outcome: no series is better than a series
// that is silently the wrong number.
func (c *MetricsCollector) sampleSelf(now time.Time) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	c.obs("core.heap_bytes", "", "", float64(ms.HeapAlloc), now)
	c.obs("core.goroutines", "", "", float64(runtime.NumGoroutine()), now)

	if rss, ok := procRSSBytes(); ok {
		// Resident set, not heap. The gap between the two is what a reader
		// actually pays for in a container memory limit.
		c.obs("core.rss_bytes", "", "", rss, now)
	}
	if pct, ok := c.procCPUPercent(now); ok {
		c.obs("core.cpu_pct", "", "", pct, now)
	}
}

// procRSSBytes reads VmRSS from /proc/self/status.
func procRSSBytes() (float64, bool) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// procCPUPercent turns the process's cumulative CPU time into a percentage of
// one core, averaged over the interval since the previous sample.
func (c *MetricsCollector) procCPUPercent(now time.Time) (float64, bool) {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	// utime and stime are fields 14 and 15, but the process NAME is field 2 and
	// may itself contain spaces and parentheses. Everything before the last ')'
	// is therefore skipped rather than split.
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(b)[i+1:])
	// After the ')' the first field is state, so utime is index 11 and stime 12.
	if len(f) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseFloat(f[11], 64)
	stime, err2 := strconv.ParseFloat(f[12], 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	// USER_HZ is 100 on every Linux this runs on; the value is clock ticks.
	total := (utime + stime) / 100

	prev, prevAt := c.lastCPUSeconds, c.lastCPUAt
	c.lastCPUSeconds, c.lastCPUAt = total, now
	if prevAt.IsZero() || total < prev {
		return 0, false
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	return (total - prev) / elapsed * 100, true
}

// samplePostgres records what the database is doing, from pg_stat_database.
//
// One row, already maintained by Postgres for its own accounting. The rates a
// reader asks for - commits per minute, cache hit ratio, rows read - are all
// deltas of the totals in that row, which is what counterSource turns them into.
func (c *MetricsCollector) samplePostgres(ctx context.Context, db *sql.DB, now time.Time) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var (
		backends                        int64
		commit, rollback                int64
		blksRead, blksHit               int64
		tupReturned, tupFetched         int64
		tupInserted, tupUpdated, tupDel int64
		deadlocks                       int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT numbackends, xact_commit, xact_rollback, blks_read, blks_hit,
		       tup_returned, tup_fetched, tup_inserted, tup_updated, tup_deleted,
		       deadlocks
		FROM pg_stat_database WHERE datname = current_database()`).Scan(
		&backends, &commit, &rollback, &blksRead, &blksHit,
		&tupReturned, &tupFetched, &tupInserted, &tupUpdated, &tupDel, &deadlocks)
	if err != nil {
		return
	}

	// A gauge: how many connections are open right now.
	c.obs("postgres.backends", "", "", float64(backends), now)

	for name, cur := range map[string]int64{
		"postgres.commits":       commit,
		"postgres.rollbacks":     rollback,
		"postgres.blocks_read":   blksRead,
		"postgres.blocks_hit":    blksHit,
		"postgres.rows_returned": tupReturned,
		"postgres.rows_fetched":  tupFetched,
		"postgres.rows_inserted": tupInserted,
		"postgres.rows_updated":  tupUpdated,
		"postgres.rows_deleted":  tupDel,
		"postgres.deadlocks":     deadlocks,
	} {
		if d, ok := c.pgCounters.delta(name, float64(cur)); ok {
			c.obs(name, "", "", d, now)
		}
	}
}

// sampleDatabaseSize records how large the data has grown. Slow-cadence: it is
// a number that moves over days, and pg_database_size walks the directory.
func (c *MetricsCollector) sampleDatabaseSize(ctx context.Context, db *sql.DB, now time.Time) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var size int64
	if err := db.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		return
	}
	c.obs("postgres.size_bytes", "", "", float64(size), now)
}

// sampleRedis records Redis' own view of itself, from INFO.
func (c *MetricsCollector) sampleRedis(ctx context.Context, now time.Time) {
	if c.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := c.redis.Info(ctx, "clients", "memory", "stats").Result()
	if err != nil {
		return
	}
	info := parseRedisInfo(raw)

	// Gauges - what is true now.
	for field, metric := range map[string]string{
		"connected_clients":         "redis.clients",
		"used_memory":               "redis.memory_bytes",
		"instantaneous_ops_per_sec": "redis.ops_per_sec",
	} {
		if v, ok := info[field]; ok {
			c.obs(metric, "", "", v, now)
		}
	}

	// Cumulative totals - recorded as deltas.
	for field, metric := range map[string]string{
		"total_commands_processed": "redis.commands",
		"total_net_input_bytes":    "redis.rx_bytes",
		"total_net_output_bytes":   "redis.tx_bytes",
		"keyspace_hits":            "redis.keyspace_hits",
		"keyspace_misses":          "redis.keyspace_misses",
		"expired_keys":             "redis.expired_keys",
		"evicted_keys":             "redis.evicted_keys",
	} {
		v, ok := info[field]
		if !ok {
			continue
		}
		if d, ok := c.redisCounters.delta(metric, v); ok {
			c.obs(metric, "", "", d, now)
		}
	}
}

// parseRedisInfo turns an INFO reply into the numeric fields it carries.
// Non-numeric values (versions, run ids) are dropped rather than coerced.
func parseRedisInfo(raw string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			out[k] = f
		}
	}
	return out
}

// sampleUserTraffic records the SHAPE of traffic across tenants: the total, the
// average, and the two extremes.
//
// Deliberately not one series per user. Per-user history is what the billing
// tables already are, and a series per tenant per bucket makes the store grow
// with the customer list rather than with the code - the one way these maps
// could become unbounded. Four series answer "what does a typical customer use
// and what does the heaviest one use", which is the question being asked.
func (c *MetricsCollector) sampleUserTraffic(now time.Time) {
	if c.store == nil {
		return
	}
	period := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := c.store.ListTrafficUsage(period)
	if err != nil || len(rows) == 0 {
		return
	}
	var total, minB, maxB float64
	for i, r := range rows {
		b := float64(r.EdgeBytes + r.RelayBytes)
		total += b
		if i == 0 || b < minB {
			minB = b
		}
		if b > maxB {
			maxB = b
		}
	}
	c.obs("platform.user_traffic_total_bytes", "", "", total, now)
	c.obs("platform.user_traffic_avg_bytes", "", "", total/float64(len(rows)), now)
	c.obs("platform.user_traffic_min_bytes", "", "", minB, now)
	c.obs("platform.user_traffic_max_bytes", "", "", maxB, now)
	c.obs("platform.billed_users", "", "", float64(len(rows)), now)
}

// samplePresence records how many people were using the platform at this
// instant, and how many streams they held.
//
// Counted by SCANning the per-user presence keys rather than by asking each
// Core replica, because a user with two tabs on two replicas is ONE user and
// summing per-replica counts would say two. See handlers/presence.go.
func (c *MetricsCollector) samplePresence(ctx context.Context, now time.Time) {
	if c.redis == nil {
		return
	}
	users := 0
	var cursor uint64
	for {
		batch, next, err := c.redis.Scan(ctx, cursor, "dylaris:presence:user:*", 500).Result()
		if err != nil {
			return
		}
		users += len(batch)
		if next == 0 {
			break
		}
		cursor = next
	}
	c.obs("platform.concurrent_users", "", "", float64(users), now)

	// Streams. More streams than users is normal (tabs).
	streams, streamKeys := 0.0, 0
	cursor = 0
	for {
		batch, next, err := c.redis.Scan(ctx, cursor, "dylaris:presence:streams:*", 200).Result()
		if err != nil {
			break
		}
		for _, k := range batch {
			streamKeys++
			if v, err := c.redis.Get(ctx, k).Float64(); err == nil {
				streams += v
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if streamKeys > 0 {
		c.obs("platform.panel_streams", "", "", streams, now)
	}

	// Replicas: fewer than expected means a Core is not serving. Counted from
	// the heartbeat every Core writes, not from the stream keys, which exist
	// only on a replica that currently holds a panel stream.
	replicas := 0
	ids, err := c.redis.SMembers(ctx, CoreIndexKey).Result()
	if err != nil {
		return
	}
	for _, id := range ids {
		if n, err := c.redis.Exists(ctx, "dylaris:core:"+id).Result(); err == nil && n > 0 {
			replicas++
		}
	}
	if replicas > 0 {
		c.obs("platform.core_replicas", "", "", float64(replicas), now)
	}
}
