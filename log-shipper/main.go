package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"dylaris-pkg/retry"

	"github.com/redis/go-redis/v9"
)

// crashStreak counts crashes inside a rolling window (see crashWindow).
type crashStreak struct {
	count int
	first time.Time // when the current streak began
}

// record folds one process exit into the streak and reports whether the
// supervisor should stop restarting.
//
// A crash that arrives more than a window after the streak began is a separate
// incident and starts a new streak, so a server that crashes a few times over a
// day while running normally in between is never given up on. Three inside one
// window is a loop.
func (c *crashStreak) record(now time.Time) (count int, giveUp bool) {
	if c.count == 0 || now.Sub(c.first) > crashWindow {
		c.count, c.first = 1, now
	} else {
		c.count++
	}
	return c.count, c.count >= maxCrashesInWindow
}

const (
	batchSize     = 50
	batchInterval = 200 * time.Millisecond
	maxStreamLen  = 5000 // console history per server; the panel pages back through it
	lineChanSize  = 512
	// Key TTL.
	//
	// Long, because the FALLBACK is worse than a stale value. Once this key
	// expires the panel shows the container metric instead, and that number is
	// known to be wrong: the platform forces Xms=Xmx and Java never returns
	// heap to the OS, so it sits at the limit for as long as the server runs.
	// An idle MC server with a large heap can easily go minutes between young
	// GCs, which is exactly when the reader is looking at a quiet server and
	// being told it is out of memory.
	//
	// The post-GC live heap is also the slowest-changing number here: on an
	// idle server it is nearly constant, so an old reading is close to right.
	// A restart does NOT inherit the previous run's value - the key is deleted
	// at startup - so the only thing this TTL still bounds is a server that
	// stopped without the shipper getting to clean up.
	heapKeyTTL = time.Hour

	// How often the RCON-filter toggle is re-read. A console setting does not
	// need to be instant, and this bounds the extra Redis load to one GET per
	// server per interval regardless of log volume.
	rconFilterRefresh = 15 * time.Second

	// Crash-loop limits. This supervisor is PID 1 of the server container, so
	// for as long as it keeps restarting a process that cannot live, the
	// container stays Up - and every layer above reads that as a healthy
	// server. The node's reconciler only ever looks at containers that are NOT
	// running, the stats collector likewise, so a JVM dying every five seconds
	// is invisible to all of them and the panel reads "online".
	//
	// Measured: a server whose jar was missing sat at "online" for eight
	// minutes while java exited instantly on a loop; nothing anywhere noticed.
	// It would have stayed that way indefinitely.
	//
	// So the supervisor gives up on a process that will not stay alive, and the
	// container exits with it. That hands the situation to the machinery that
	// already exists for a dead container: the node reconciler retries it, and
	// on repeated failure writes the reconcile_failed key Core surfaces. A
	// container that is Up means a server that is running again.
	//
	// Three crashes inside one window is enough to conclude the server cannot
	// run; a fourth start would only repeat the same failure.
	//
	// The window is what keeps that from punishing a server that merely crashes
	// occasionally. Counting CONSECUTIVE crashes and resetting after a run of
	// some minimum length looks equivalent and is not: with a 60s reset, a server
	// that survives 61 seconds every time resets forever and is restarted
	// forever. Anchoring the streak to wall-clock instead means three crashes an
	// hour apart are three separate incidents, and three inside a quarter of an
	// hour are a loop.
	//
	// 15 minutes is chosen against start time. A normal server is up in well
	// under a minute and even a large modpack in a few, so three failures inside
	// fifteen minutes cannot be three healthy lifetimes. Erring long is the safe
	// direction: the cost of a too-long window is that a genuinely broken server
	// flaps a while longer, which is now visible in the panel, while a too-short
	// one takes a working server away from its owner.
	//
	// Mirrored in node/reconciler.go so an operator reading either file sees one
	// policy.
	maxCrashesInWindow = 3
	crashWindow        = 15 * time.Minute
)

// gcLineRegex matches the GC summary lines emitted by both Java 9+
// unified logging (`-Xlog:gc`) and the legacy Java-8 `-XX:+PrintGCDetails`
// format. Captures: before-GC, after-GC, total -- each with a K or M
// unit suffix.
//
//	Java 17 G1: "[...][info][gc] GC(0) Pause Young (Normal) ... 256M->50M(2048M) 12.345ms"
//	Java 8 PG:  "[GC (Allocation Failure)  256000K->50000K(2048000K), 0.012345 secs]"
var gcLineRegex = regexp.MustCompile(`(\d+)([KM])->(\d+)([KM])\((\d+)([KM])\)`)

// javaLegacyGCLine matches the start of a Java 8 -XX:+PrintGCDetails record,
// optionally behind the stamps -XX:+PrintGCDateStamps and +PrintGCTimeStamps
// put in front of it ("2026-08-27T12:00:00.000+0000: 1.234: [GC ...").
//
// Anchored at the START on purpose - that is what a player cannot forge. See
// isGCSummaryLine.
var javaLegacyGCLine = regexp.MustCompile(`^(?:[0-9T:.+-]+: )*\[(?:Full )?GC[ (]`)

// isGCSummaryLine reports whether a line is the JVM's OWN garbage-collector
// output rather than something the server printed.
//
// The heap number used to be pulled out of every line that merely contained the
// shape "<n>M-><n>M(<n>M)", and a Minecraft server prints whatever its players
// type. "1M->9999M(9999M)" in chat therefore set the live-heap metric the panel
// charts. It sticks, too: the value is only replaced by the next real GC, and
// the case the metric exists for is an idle server, which is exactly the one
// that can go minutes between young GCs.
//
// Everything a player can reach arrives through Minecraft's logger, so it always
// carries that logger's prefix; the JVM writes GC output to raw stdout with no
// prefix at all. Matching the two GC FORMATS is the closed set - the platform
// chooses what GC logging it injects - where "is this a Minecraft log line" is
// open-ended and differs between vanilla and Paper.
func isGCSummaryLine(line string) bool {
	return isUnifiedGCLine(line) || javaLegacyGCLine.MatchString(line)
}

// parseHeapAfterGC extracts the post-GC live-heap size (in MB) from a
// JVM GC log line. Returns 0 + false if no GC summary is on the line.
func parseHeapAfterGC(line string) (int64, bool) {
	if !isGCSummaryLine(line) {
		return 0, false
	}
	m := gcLineRegex.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	val, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return 0, false
	}
	if m[4] == "K" {
		val /= 1024
	}
	// Reject zero/negative: a 0 MB heap "value" right after a Full GC
	// would publish a misleading dip to zero in the live chart. Skipping
	// it lets the previous value stand until a meaningful GC happens.
	if val <= 0 {
		return 0, false
	}
	return val, true
}

// isUnifiedGCLine reports whether a stdout line is a JVM unified-logging
// GC summary that we should hide from the user-facing console. The lines
// only exist because the platform injects `-Xlog:gc::utctime,level,tags`
// to feed the live heap metric -- the operator never asked for them and
// they drown out the actual MC server output. We still parse the heap
// number out before filtering, so the metric stays accurate.
//
// The unified-logging format is `[<timestamp>][<level>][gc] ...` (and
// `][gc,heap]`, `][gc,start]` etc. variants). Substring `][gc` is unique
// enough; MC's own log4j format uses `[Server thread/INFO]:` so there's
// no overlap. Java 8's legacy `[GC (...)]` format is not filtered: we
// don't inject -Xlog on Java 8 anyway, so anything in that shape is
// user-requested and should pass through.
func isUnifiedGCLine(line string) bool {
	return strings.Contains(line, "][gc")
}

// isRconNoiseLine reports whether a line is Minecraft's per-RCON-connection
// chatter rather than anything about the server.
//
// The panel polls RCON (the online-player list every 10s, plus status reads), and
// vanilla/Paper logs a thread start AND a shutdown for every one of those
// connections. The console fills with
//
//	[12:00:00] [RCON Listener #1/INFO]: Thread RCON Client /172.18.0.1 started
//	[12:00:00] [RCON Client /172.18.0.1 #4/INFO]: Thread RCON Client /172.18.0.1 shutting down
//
// at the panel's polling rate, which pushes real output out of the 1000-line
// stream. Matched on the LOGGER THREAD ("[RCON ") rather than the message text, so
// a chat message that happens to contain the word RCON is not swallowed; the
// bracket is what makes it a thread name and not user content.
//
// Opt-in: it hides output the operator might want, so the default is off and the
// per-server toggle turns it on.
func isRconNoiseLine(line string) bool {
	return strings.Contains(line, "[RCON Listener") ||
		strings.Contains(line, "[RCON Client") ||
		strings.Contains(line, "Thread RCON Client")
}

// rconFilterFlag tracks the per-server "hide RCON connection noise" toggle.
// Read on EVERY log line, refreshed on a timer: a Redis GET per line would put
// one round-trip in front of every line a busy server prints.
type rconFilterFlag struct{ on atomic.Bool }

// watch keeps the flag current from Redis. Written by Core when the toggle
// changes (dylaris:server:<uuid>:log_filter_rcon) and refreshed by Core's status
// watcher, same as the edge-MOTD keys. A read error leaves the last known value
// rather than flipping the filter: losing Redis should not suddenly change what
// the console shows.
func (f *rconFilterFlag) watch(ctx context.Context, rdb *redis.Client, key string) {
	poll := func() {
		v, err := rdb.Get(ctx, key).Result()
		if err == redis.Nil {
			f.on.Store(false) // key absent is a definite "off", not an error
			return
		}
		if err != nil {
			return
		}
		f.on.Store(v == "true" || v == "1")
	}
	poll()
	ticker := time.NewTicker(rconFilterRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getSecret resolves a secret with Docker/Portainer secrets support: contents of
// "<key>_FILE" (trimmed) -> plain "<key>" -> fallback. Lets operators mount a
// secret at a path instead of passing it in plain env.
func getSecret(key, fallback string) string {
	if path := os.Getenv(key + "_FILE"); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return v
			}
			log.Printf("config: %s_FILE (%s) is empty; falling back to %s", key, path, key)
		} else {
			log.Printf("config: failed to read %s_FILE (%s): %v; falling back to %s", key, path, err, key)
		}
	}
	return getEnv(key, fallback)
}

// uuidRegex / subServerRegex validate the env vars that get interpolated into
// Redis keys. Only Node sets these today, but unvalidated input could collide
// key namespaces (e.g. SUB_SERVER=foo:logs) or inject separators.
//
// serverUUID is NOT a canonical UUID: the panel mints server ids as
// "<ownerUUID>_<random>", so the check accepts any safe identifier (hex/alnum
// plus '-' and '_', length-bounded) - the goal is to reject Redis-key injection
// characters (':' '/' whitespace), not to mandate RFC-4122. This mirrors the
// core-side rule in platform/pkg/validate (ServerUUID); a strict canonical regex
// here rejected every panel-created server and crash-looped its container.
var (
	uuidRegex      = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)
	subServerRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func connectRedis() *redis.Client {
	addr := getEnv("REDIS_ADDR", "localhost:6379")
	user := getEnv("REDIS_USER", "")
	pass := getSecret("REDIS_PASS", "")
	dbStr := getEnv("REDIS_DB", "0")
	db, _ := strconv.Atoi(dbStr)

	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Username: user,
		Password: pass,
		DB:       db,
	})

	// Reconnect on the platform's shared schedule (12x5s, then every 30s). Never
	// gives up: the Minecraft server is already running by the time this returns
	// on a restart, so a Redis that is merely slow to come back must not take
	// the container down with it.
	var bo retry.Backoff
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := rdb.Ping(ctx).Err()
		cancel()
		if err == nil {
			log.Printf("log-shipper: connected to Redis at %s", addr)
			return rdb
		}
		wait := bo.Next()
		log.Printf("log-shipper: Redis not reachable (%v), retrying in %s...", err, wait)
		time.Sleep(wait)
	}
}

// applyCarriageReturns simulates terminal CR behavior.
// \r resets the cursor to column 0; the following text overwrites from the start.
//
// Written in place rather than by splitting on \r and re-concatenating. The
// split version was quadratic: every segment shorter than what came before
// rebuilt the whole string, so one line holding a long run followed by many
// one-character runs cost tens of gigabytes of copying. That is not a slow log
// line - while it runs nothing drains this reader, so the JVM's stdout pipe
// fills and the Minecraft server blocks on its next log write. Anything that
// reaches the console can carry a \r: a mod, a plugin, a player name.
func applyCarriageReturns(line string) string {
	if !strings.ContainsRune(line, '\r') {
		return line
	}
	buf := make([]byte, 0, len(line))
	col := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '\r' {
			col = 0
			continue
		}
		if col < len(buf) {
			buf[col] = line[i]
		} else {
			buf = append(buf, line[i])
		}
		col++
	}
	return string(buf)
}

// maxLineBytes caps ONE console line. Over it the line is truncated and marked;
// reading continues either way.
const maxLineBytes = 1 << 20

const lineTruncatedMarker = " [dylaris: line truncated at 1MB]"

// scanLines reads from r line by line and sends each line to ch.
//
// A bufio.Reader and not a bufio.Scanner, deliberately. A Scanner that meets a
// line longer than its buffer sets ErrTooLong, and from then on EVERY Scan
// returns false - the failure is permanent, not per-line. This function then
// returned, so nothing read the pipe again, and because this process is PID 1
// of the Minecraft container the JVM blocked on its next write to a full stdout
// pipe: one oversized log line hung the customer's server, with the console
// frozen at whatever it last showed. main compounded it - scanWG.Wait() waits
// for both readers, and the surviving one never finishes either.
//
// The old code reported "a log line exceeded 1MB and was dropped", which is how
// it looked from in here and not what happened.
func scanLines(r io.Reader, ch chan<- string) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		line      []byte
		truncated bool
	)
	for {
		// ReadSlice returns ErrBufferFull for a line longer than the buffer,
		// which is the ordinary case here rather than an error: keep taking
		// chunks until the newline arrives.
		chunk, err := br.ReadSlice('\n')
		if room := maxLineBytes - len(line); room > 0 {
			if len(chunk) > room {
				line, truncated = append(line, chunk[:room]...), true
			} else {
				line = append(line, chunk...)
			}
		} else if len(chunk) > 0 {
			truncated = true
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		// err == nil means the newline landed. On EOF a partial last line is
		// still worth shipping, but an empty one is not a line at all.
		if err == nil || len(line) > 0 {
			out := strings.TrimSuffix(string(line), "\n")
			if truncated {
				out += lineTruncatedMarker
			}
			ch <- applyCarriageReturns(out)
			line, truncated = line[:0], false
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("log-shipper: read error: %v", err)
			}
			return
		}
	}
}

// lossMarker is written into the console once, ahead of the replayed lines, when
// an outage lasted longer than the buffer could cover. Losing console output is
// survivable; losing it SILENTLY is the actual bug - a reader who sees a gap
// with no explanation assumes the server went quiet.
const lossMarker = "[dylaris] %d console lines were lost while the log service was unreachable"

const (
	// replayMaxLines caps the buffer at what the destination can actually hold.
	// The stream is trimmed to maxStreamLen, so buffering more only hands Redis
	// lines it trims on arrival.
	// The newest lines are the ones kept, which is what a reader wants after an
	// outage.
	replayMaxLines = maxStreamLen

	// replayMaxBytes is the second cap, and the one that matters for safety.
	// This process is PID 1 INSIDE the Minecraft container and shares the
	// container's memory limit with the JVM - container RAM is
	// "requested + 512MB" and the heap is pinned by Xms=Xmx, so there is no
	// slack to borrow. A single line may be up to 1MB (the scanner's cap), so
	// the line cap alone would allow a gigabyte. An unbounded buffer here does
	// not merely waste memory: it can get the customer's server OOM-killed, and
	// it grows exactly when something is already wrong.
	//
	// 8MB is far above normal console output for replayMaxLines lines (~120
	// bytes each) and far below anything that threatens the JVM.
	replayMaxBytes = 8 << 20
)

// replayBuffer holds console lines Redis has refused, oldest first, so a blip in
// the log service delays output instead of discarding it. On overflow the OLDEST
// lines go and are counted, because after an outage the newest output is what
// tells the reader what state the server is in now.
type replayBuffer struct {
	lines   []string
	bytes   int
	dropped int
}

// add appends a batch and evicts from the front until both caps hold.
func (r *replayBuffer) add(lines []string) {
	for _, l := range lines {
		r.lines = append(r.lines, l)
		r.bytes += len(l)
	}
	for len(r.lines) > 0 && (len(r.lines) > replayMaxLines || r.bytes > replayMaxBytes) {
		r.bytes -= len(r.lines[0])
		r.lines[0] = "" // release the string; the slice header keeps the array alive
		r.lines = r.lines[1:]
		r.dropped++
	}
}

func (r *replayBuffer) empty() bool { return len(r.lines) == 0 && r.dropped == 0 }

// pending returns what should be written next, with the loss marker prepended
// when lines were evicted. It does NOT clear the buffer - the caller clears only
// after Redis has accepted the write, so a failed attempt loses nothing.
func (r *replayBuffer) pending() []string {
	if r.dropped == 0 {
		return r.lines
	}
	out := make([]string, 0, len(r.lines)+1)
	out = append(out, fmt.Sprintf(lossMarker, r.dropped))
	return append(out, r.lines...)
}

func (r *replayBuffer) clear() { r.lines, r.bytes, r.dropped = nil, 0, 0 }

// heapLatest holds the most recent post-GC heap reading (MB) until a flush can
// publish it. Latest-wins: between two flushes a busy server emits many GC
// lines, and only the newest is worth writing - the key holds one value.
//
// It exists so the line-reading path does no Redis I/O at all. See the flush
// comment in shipLogs for what the per-line write cost.
type heapLatest struct{ mb int64 }

func (h *heapLatest) note(mb int64) { h.mb = mb }

// take returns the pending reading and clears it. ok is false when nothing new
// arrived since the last take, so a flush with no GC activity issues no write.
func (h *heapLatest) take() (int64, bool) {
	if h.mb <= 0 {
		return 0, false
	}
	mb := h.mb
	h.mb = 0
	return mb, true
}

// shipLogs batches log lines and writes them to the Redis Stream.
// Flushes every 200ms or when 50 lines accumulate, whichever comes first.
// Side effect: also scans each line for JVM GC summaries and updates a
// per-server Redis key with the latest post-GC heap size so the node
// stats collector can surface live heap usage to the panel.
func shipLogs(ctx context.Context, rdb *redis.Client, streamKey, heapKey string, lineCh <-chan string, rconFilter *rconFilterFlag) {
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	var (
		buf      []string
		replay   replayBuffer
		heap     heapLatest
		bo       retry.Backoff
		nextTry  time.Time // zero = attempt immediately
		degraded bool      // true between the first failed write and the recovery
	)

	// flush moves whatever has accumulated into the replay buffer and tries to
	// write the buffer out. While Redis is unreachable the attempt is spaced by
	// the shared retry schedule rather than repeated on every 200ms tick, so an
	// outage does not put a failing round-trip in front of every batch.
	//
	// force ignores that spacing; used on shutdown, where there is no later
	// attempt to defer to.
	flush := func(now time.Time, force bool) {
		if len(buf) > 0 {
			replay.add(buf)
			buf = buf[:0]
		}
		if !force && now.Before(nextTry) {
			return
		}

		// The latest post-GC heap reading, written HERE rather than on the line
		// that produced it.
		//
		// rdb.Set is a synchronous round-trip. Doing it per GC line put one in
		// front of the loop whose whole job is to keep draining lineCh, and -
		// unlike the stream write below - it obeyed no retry spacing at all.
		// With Redis unreachable that is not a slow metric, it is a stalled
		// reader: lineCh (512) fills, both scanners block, the JVM's stdout pipe
		// fills, and the Minecraft server itself blocks writing a log line. The
		// replay buffer exists precisely so a log outage delays output instead
		// of costing anything, and this one unspaced call walked around it.
		//
		// Coalescing also drops steady-state round-trips from one per GC line to
		// at most one per batch. A dropped reading is harmless and always was -
		// the next GC replaces it.
		heapFailed := false
		if heapKey != "" {
			if mb, ok := heap.take(); ok {
				heapFailed = rdb.Set(ctx, heapKey, mb, heapKeyTTL).Err() != nil
			}
		}

		if replay.empty() {
			// Nothing to ship, but a failed heap write still means Redis is
			// unreachable. Fall in behind the same spacing, or a server whose
			// only output is GC lines - which are filtered out of the stream, so
			// they never produce one - would retry a blocking write on every
			// 200ms tick, with no stream write ever there to set the backoff.
			if heapFailed && !force && ctx.Err() == nil {
				nextTry = now.Add(bo.Next())
			}
			return
		}
		lines := replay.pending()
		write := func() error {
			// Always a FRESH pipeline: go-redis resets a pipeline after Exec, so
			// re-running the same object would send nothing and look like a
			// silent success.
			pipe := rdb.Pipeline()
			for _, line := range lines {
				pipe.XAdd(ctx, &redis.XAddArgs{
					Stream: streamKey,
					MaxLen: maxStreamLen,
					Approx: true,
					Values: map[string]interface{}{"line": line},
				})
			}
			_, err := pipe.Exec(ctx)
			return err
		}
		err := write()
		if err != nil && ctx.Err() == nil {
			// One immediate retry before falling back to the schedule. The
			// common failure is a pooled connection Redis has already closed,
			// where the second attempt succeeds at once - without this, that
			// costs the console a full retry interval for nothing.
			err = write()
		}
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down; the buffer dies with the process
			}
			wait := bo.Next()
			nextTry = now.Add(wait)
			// Logged once per outage, not once per failed attempt: this file is
			// PID 1 of the container, so its own log is the only place an
			// operator can see this, and a line every 200ms would bury it.
			if !degraded {
				degraded = true
				log.Printf("log-shipper: Redis write failed, buffering console output (retry in %s): %v", wait, err)
			}
			return
		}
		if degraded {
			log.Printf("log-shipper: Redis reachable again, replayed %d buffered lines", len(lines))
			degraded = false
		}
		// Reset on success, not only at startup: a Redis that flaps gets the
		// dense retry schedule again each time it drops out.
		bo.Reset()
		nextTry = time.Time{}
		replay.clear()
	}

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				// The process has exited and the scanners have drained. This is
				// the last chance to get the crash output out, so ignore the
				// retry spacing.
				flush(time.Now(), true)
				return
			}
			// JVM heap accounting: pulls "100M->50M(2048M)" out of GC
			// summary lines and stores the post-GC value. Container-level
			// stats from Docker are anon-RSS = Xms (since we force
			// Xms=Xmx) and so look pinned at the limit forever; this is
			// the actual live heap.
			if heapKey != "" {
				if mb, ok := parseHeapAfterGC(line); ok {
					// Recorded only. The write happens in flush, which is the
					// one place that respects the retry schedule; this path
					// must stay free of Redis round-trips so it keeps draining
					// lineCh. Stale values fall out via TTL.
					heap.note(mb)
				}
			}
			// Hide JVM GC log noise from the user console. Parsing above
			// still runs so the heap chart keeps updating; only the
			// stream-shipping is skipped. Without -Xlog the JVM is silent
			// here anyway, so this is purely about not exposing platform-
			// internal logs to the operator.
			if isUnifiedGCLine(line) {
				continue
			}
			// Per-server toggle, off by default. Dropped here rather than at
			// read time so the filter can be turned on and off on a running
			// server without restarting it.
			if rconFilter != nil && rconFilter.on.Load() && isRconNoiseLine(line) {
				continue
			}
			buf = append(buf, line)
			if len(buf) >= batchSize {
				flush(time.Now(), false)
			}
		case now := <-ticker.C:
			flush(now, false)
		case <-ctx.Done():
			flush(time.Now(), true)
			return
		}
	}
}

// forwardInput reads commands from the Redis input queue and writes them to Java stdin.
func forwardInput(ctx context.Context, rdb *redis.Client, inputKey string, stdin io.WriteCloser) {
	for {
		result, err := rdb.BLPop(ctx, 2*time.Second, inputKey).Result()
		if err == redis.Nil {
			// Timeout — try again
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("log-shipper: BLPop error: %v, retrying in 1s...", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if len(result) >= 2 {
			// Strip embedded CR/LF so one queue entry can't smuggle extra
			// console commands into the server stdin (newline injection).
			line := strings.ReplaceAll(strings.ReplaceAll(result[1], "\r", ""), "\n", "")
			// Cap at 1KB: a single console command is never legitimately larger.
			if len(line) > 1024 {
				line = line[:1024]
			}
			if _, err := fmt.Fprint(stdin, line+"\n"); err != nil {
				log.Printf("log-shipper: stdin write error: %v", err)
				return
			}
		}
	}
}

// requestStop is what happens when this container is asked to stop from the
// outside. It sets the flag FIRST and only then asks the server to quit, so the
// restart loop cannot see the exit before it knows the exit was wanted.
//
// Without this there was no signal handling at all, and Go's default action for
// SIGTERM is to terminate. This process is PID 1 of the Minecraft container, so
// "docker stop" killed the supervisor instantly and the JVM - a child in that
// PID namespace - was SIGKILLed with it: no save, no clean shutdown. The node
// normally stops a server by pushing "stop" onto the input queue and only falls
// back to ContainerStop, but every path that skips straight to ContainerStop
// (RecreateWithCommand, PowerAction "stop", a node or host restart) landed on
// the hard kill.
//
// Docker's stop timeout still bounds this: the node passes 15-30s and SIGKILL
// follows, so the worst case is exactly what happened before.
func requestStop(stopping *atomic.Bool, stdin io.Writer) {
	stopping.Store(true)
	log.Print("log-shipper: stop signal received, asking the server to shut down cleanly")
	if _, err := fmt.Fprint(stdin, "stop\n"); err != nil {
		log.Printf("log-shipper: could not write stop to stdin: %v", err)
	}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: log-shipper <command> [args...]\nExample: log-shipper java -Xmx2048M -jar /data/server/server.jar nogui")
	}

	serverUUID := getEnv("SERVER_UUID", "")
	if serverUUID == "" {
		log.Fatal("log-shipper: SERVER_UUID environment variable is required")
	}
	if !uuidRegex.MatchString(serverUUID) {
		log.Fatalf("log-shipper: SERVER_UUID %q is not a valid UUID", serverUUID)
	}

	subServer := getEnv("SUB_SERVER", "")
	if subServer != "" && !subServerRegex.MatchString(subServer) {
		log.Fatalf("log-shipper: SUB_SERVER %q must match [A-Za-z0-9_-]{1,64}", subServer)
	}
	streamKey := fmt.Sprintf("dylaris:server:%s:logs", serverUUID)
	if subServer != "" {
		streamKey = fmt.Sprintf("dylaris:server:%s:logs:%s", serverUUID, subServer)
	}
	// Heap key holds the latest post-GC live-heap size (in MB) parsed
	// off the JVM's stdout. Only one sub-server is active at a time and
	// the container restarts on switch, so a single unified key is
	// enough -- a fresh log-shipper instance overwrites stale values.
	heapKey := fmt.Sprintf("dylaris:server:%s:java-heap", serverUUID)
	inputKey := fmt.Sprintf("dylaris:server:%s:input", serverUUID)
	// Per-server console toggle, written by Core. Not an env var: an env change
	// needs the container recreated, and this has to be flippable while the
	// server runs.
	rconFilterKey := fmt.Sprintf("dylaris:server:%s:log_filter_rcon", serverUUID)

	rdb := connectRedis()
	defer rdb.Close()

	// Drop the previous run's heap reading. A new container can mean a new
	// -Xmx, and the value only becomes right again at the first GC - which on a
	// large heap is not immediate. Clearing it here is what makes the long
	// heapKeyTTL safe: nothing inherits a value across a restart.
	rdb.Del(context.Background(), heapKey)

	rconFilter := &rconFilterFlag{}

	// Check if working directory exists (may have been renamed by user)
	cwd, _ := os.Getwd()
	if _, err := os.Stat(cwd); err != nil && os.IsNotExist(err) {
		msg := fmt.Sprintf("[log-shipper] Working directory %s does not exist — server folder may have been renamed. Container will stop.", cwd)
		log.Print(msg)
		rdb.XAdd(context.Background(), &redis.XAddArgs{
			Stream: streamKey,
			MaxLen: maxStreamLen,
			Approx: true,
			Values: map[string]interface{}{"line": msg},
		})
		os.Exit(0)
	}

	stopKey := fmt.Sprintf("dylaris:server:%s:stop-requested", serverUUID)

	// Buffered: signal delivery never blocks, and one pending SIGTERM is all
	// this needs - the second one from an impatient operator changes nothing.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)

	// Restart loop: re-launches Java on crash, exits on manual stop, and gives
	// up once the process has proven it cannot stay alive (see
	// maxConsecutiveCrashes).
	var streak crashStreak
	var stopping atomic.Bool
	for {
		javaCmd := exec.Command(os.Args[1], os.Args[2:]...)
		javaCmd.Env = append(os.Environ(), "TERM=xterm-256color")
		javaCmd.Stdin = nil // stdin managed via pipe below

		stdoutPipe, err := javaCmd.StdoutPipe()
		if err != nil {
			log.Fatalf("log-shipper: StdoutPipe error: %v", err)
		}
		stderrPipe, err := javaCmd.StderrPipe()
		if err != nil {
			log.Fatalf("log-shipper: StderrPipe error: %v", err)
		}
		stdinPipe, err := javaCmd.StdinPipe()
		if err != nil {
			log.Fatalf("log-shipper: StdinPipe error: %v", err)
		}

		if err := javaCmd.Start(); err != nil {
			log.Fatalf("log-shipper: failed to start process %q: %v", os.Args[1], err)
		}
		log.Printf("log-shipper: started PID %d: %s", javaCmd.Process.Pid, strings.Join(os.Args[1:], " "))

		ctx, cancel := context.WithCancel(context.Background())

		lineCh := make(chan string, lineChanSize)

		var scanWG sync.WaitGroup
		scanWG.Add(2)

		go func() { defer scanWG.Done(); scanLines(stdoutPipe, lineCh) }()
		go func() { defer scanWG.Done(); scanLines(stderrPipe, lineCh) }()

		go forwardInput(ctx, rdb, inputKey, stdinPipe)
		go rconFilter.watch(ctx, rdb, rconFilterKey)
		go shipLogs(ctx, rdb, streamKey, heapKey, lineCh, rconFilter)

		// Tied to this iteration's ctx, so at most one of these is ever waiting
		// on sigCh and it always holds the CURRENT process's stdin.
		go func() {
			select {
			case <-sigCh:
				requestStop(&stopping, stdinPipe)
			case <-ctx.Done():
			}
		}()

		// Wait for Java process to exit
		if err := javaCmd.Wait(); err != nil {
			log.Printf("log-shipper: process exited with error: %v", err)
		}

		// Wait for scanners to drain, then close lineCh so shipLogs flushes.
		scanWG.Wait()
		close(lineCh)
		time.Sleep(300 * time.Millisecond)
		cancel()

		exitCode := 0
		if javaCmd.ProcessState != nil {
			exitCode = javaCmd.ProcessState.ExitCode()
		}

		// Check if stop was requested via WebUI
		_, stopErr := rdb.Get(context.Background(), stopKey).Result()
		if stopErr == nil {
			// Stop flag exists -> clean exit (manual stop)
			rdb.Del(context.Background(), stopKey)
			log.Printf("log-shipper: stop requested, exiting cleanly")
			os.Exit(exitCode)
		}

		// A stop signal is as deliberate as the stop-requested key, and it is the
		// one the key cannot cover: it comes from Docker, not from Core. Checked
		// before the crash accounting so a server that was mid-crash when the
		// stop arrived is not also counted as a crash loop.
		if stopping.Load() {
			log.Printf("log-shipper: exiting after the stop signal (code %d)", exitCode)
			os.Exit(exitCode)
		}

		// Exit code 0 without stop flag = normal shutdown (e.g. /stop in-game)
		if exitCode == 0 {
			log.Printf("log-shipper: process exited normally (code 0), exiting")
			os.Exit(0)
		}

		crashes, giveUp := streak.record(time.Now())

		if giveUp {
			msg := fmt.Sprintf("[Server crashed %d times within %s (last exit code %d) - giving up. "+
				"The container is stopping so this shows as a failure instead of appearing online. "+
				"Check the log above for the reason, then start the server again once it is fixed.]",
				crashes, crashWindow, exitCode)
			log.Printf("log-shipper: %d crashes within %s, exiting with code %d so the container does not stay Up on a dead server", crashes, crashWindow, exitCode)
			rdb.XAdd(context.Background(), &redis.XAddArgs{
				Stream: streamKey,
				MaxLen: maxStreamLen,
				Approx: true,
				Values: map[string]interface{}{"line": msg},
			})
			// Give the stream write a moment to land before the container dies.
			time.Sleep(200 * time.Millisecond)
			os.Exit(exitCode)
		}

		// Crash recovery: restart after delay
		log.Printf("log-shipper: process crashed (exit %d), restarting in 5s... (%d/%d within %s)", exitCode, crashes, maxCrashesInWindow, crashWindow)
		rdb.XAdd(context.Background(), &redis.XAddArgs{
			Stream: streamKey,
			MaxLen: maxStreamLen,
			Approx: true,
			Values: map[string]interface{}{"line": fmt.Sprintf("[Server crashed (exit code %d) - restarting automatically (%d/%d)...]", exitCode, crashes, maxCrashesInWindow)},
		})
		time.Sleep(5 * time.Second)
	}
}
