// Package queue provides at-least-once work queues on top of Redis Streams +
// consumer groups. It replaces the previous RPUSH/BLPOP lists, which silently
// LOST a message if a worker died between BLPOP and finishing the work (the
// item was already popped, with no redelivery).
//
// Delivery model: a producer XADDs a message; a Consumer reads it through a
// consumer group (XREADGROUP), runs the handler, and only then XACKs. A message
// that is delivered but never ACKed (worker crash / SIGTERM mid-work) stays in
// the group's Pending Entries List and is re-delivered on the next run: the
// consumer reprocesses its own pending entries, and XAUTOCLAIM grabs entries
// orphaned by a dead/renamed consumer.
//
// At-least-once plus a per-message dedup marker gives effective once-processing
// for the common lost-ACK case. Handlers must still tolerate a rare double
// delivery (most node command handlers already do: create force-removes first,
// delete ignores not-found, start/stop/restart are idempotent). A message that
// keeps failing is dead-lettered after MaxDeliveries so it can't block the queue.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultMaxLen is the approximate cap on stream length (XADD MAXLEN ~). Streams
// are work queues, not history; processed+acked entries are trimmed off the tail.
const DefaultMaxLen = 10000

// Publish appends payload to the stream as a single "data" field and approx-caps
// the stream length. Returns the new entry ID. The payload is whatever the
// previous RPUSH pushed (e.g. a JSON-encoded command), so the wire format is
// unchanged from the consumer's point of view.
func Publish(ctx context.Context, rdb *redis.Client, stream string, payload []byte) (string, error) {
	return rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		MaxLen: DefaultMaxLen,
		Approx: true,
		Values: map[string]interface{}{"data": payload},
	}).Result()
}

// Handler processes one message payload. Returning nil ACKs the message;
// returning an error leaves it pending for redelivery (until MaxDeliveries).
type Handler func(ctx context.Context, data []byte) error

// Consumer reads a single stream as one named member of a consumer group.
type Consumer struct {
	rdb    *redis.Client
	stream string
	group  string
	name   string

	// Block is how long XREADGROUP blocks waiting for new entries.
	Block time.Duration
	// Count is the max batch size per read.
	Count int64
	// ClaimMinIdle: pending entries idle longer than this are claimed from a
	// dead/other consumer via XAUTOCLAIM.
	ClaimMinIdle time.Duration
	// DedupTTL is how long a processed-marker lives (dedup window for redelivery).
	DedupTTL time.Duration
	// MaxDeliveries: after this many failed attempts a message is dead-lettered
	// (moved to "<stream>:dead") and ACKed, so a poison message can't wedge the
	// queue.
	MaxDeliveries int64
	// Concurrency is the number of messages processed in parallel. 1 (default)
	// = strictly sequential. Set higher for independent commands (e.g. node
	// server commands); keep 1 where ordering/exclusivity matters (e.g. the
	// migration orchestrator, which serializes via per-server locks).
	Concurrency int

	// inflight holds the ids this process is running RIGHT NOW. Recovery reads
	// (ownPending, staleClaimed) list pending entries, and "pending" includes
	// entries whose handler simply has not finished yet - so without this they
	// re-run work that is still in progress, in parallel with itself. See
	// beginInflight.
	mu       sync.Mutex
	inflight map[string]struct{}
}

// beginInflight claims id for this process, reporting false when it is already
// being handled here. endInflight releases it.
//
// This is what keeps recovery from racing live work. A pending entry means
// "delivered, not yet ACKed", which covers both of "the worker died" and "the
// worker is still busy" - and recovery cannot tell them apart from Redis alone.
// The dedup marker does not help either: it is written AFTER the handler
// returns, so a handler that is still running looks exactly like one that never
// ran.
//
// Found live: a node "stop" takes over 5 seconds (it sends save-all, waits, then
// gives the server up to 30s to exit), and Block is 5 seconds - so the read
// timed out mid-handler, the idle tick called recoverPending, and the same stop
// ran a second time concurrently. The second one's 30s deadline then expired and
// SIGKILLed a Minecraft server that the first one was still shutting down
// cleanly. The same window applies to every command slower than Block: install,
// backup, restore, migrate_storage.
func (c *Consumer) beginInflight(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight == nil {
		c.inflight = make(map[string]struct{})
	}
	if _, busy := c.inflight[id]; busy {
		return false
	}
	c.inflight[id] = struct{}{}
	return true
}

func (c *Consumer) endInflight(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, id)
}

// NewConsumer builds a Consumer with sane defaults. stream is the Redis key,
// group is shared by all members, name identifies this member (stable across
// restarts so its pending entries are recovered, e.g. the node id).
func NewConsumer(rdb *redis.Client, stream, group, name string) *Consumer {
	return &Consumer{
		rdb:           rdb,
		stream:        stream,
		group:         group,
		name:          name,
		Block:         5 * time.Second,
		Count:         16,
		ClaimMinIdle:  60 * time.Second,
		DedupTTL:      24 * time.Hour,
		MaxDeliveries: 5,
		Concurrency:   1,
	}
}

func (c *Consumer) doneKey(id string) string     { return c.stream + ":done:" + id }
func (c *Consumer) attemptsKey(id string) string { return c.stream + ":attempts:" + id }
func (c *Consumer) deadKey() string              { return c.stream + ":dead" }

// EnsureGroup creates the consumer group (and the stream) if absent. Idempotent.
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	// "$" = only new messages for a freshly created group; existing entries are
	// not replayed to a brand-new group (matches the old fire-and-forward
	// semantics — only the group's own pending is ever reprocessed).
	return c.ensureGroupAt(ctx, "$")
}

// ensureGroupAt creates the group (and stream) at startID if absent. Idempotent
// (BUSYGROUP is not an error). "$" is the startup position (ignore old history);
// "0" is the self-heal position after the group vanished, which replays
// everything still in the stream so messages XADDed while the group was missing
// are delivered rather than lost.
func (c *Consumer) ensureGroupAt(ctx context.Context, startID string) error {
	err := c.rdb.XGroupCreateMkStream(ctx, c.stream, c.group, startID).Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Run blocks until ctx is cancelled, processing messages with up to
// Concurrency workers. On start and whenever the read times out it also
// reclaims stale pending (XAUTOCLAIM) and reprocesses its own pending entries,
// so a crash mid-work is recovered. Entries this process is still handling are
// skipped by that recovery (see beginInflight) - "pending" alone does not mean
// "abandoned", and re-running live work in parallel with itself is what the
// recovery pass used to do to any handler slower than Block. The worker pool
// gives natural back-pressure: when all workers are busy, Run stops reading ">"
// so undelivered entries simply wait durably in the stream.
func (c *Consumer) Run(ctx context.Context, handler Handler) error {
	if err := c.EnsureGroup(ctx); err != nil {
		return err
	}

	workers := c.Concurrency
	if workers < 1 {
		workers = 1
	}
	work := make(chan redis.XMessage)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range work {
				c.process(ctx, m, handler)
				c.endInflight(m.ID)
			}
		}()
	}
	defer func() { close(work); wg.Wait() }()

	// dispatch claims each entry BEFORE handing it over, so a recovery pass that
	// lists it again while it waits for a worker skips it (beginInflight).
	dispatch := func(msgs []redis.XMessage) bool {
		for _, m := range msgs {
			if !c.beginInflight(m.ID) {
				continue
			}
			select {
			case work <- m:
			case <-ctx.Done():
				c.endInflight(m.ID)
				return false
			}
		}
		return true
	}
	// Recovered entries go through the pool like live ones. Handled here, in
	// the reading goroutine, a recovered backup or restore stopped this
	// consumer reading anything else for as long as it ran - hours for an
	// upload - and ran beside the pool, past a Concurrency of 1.
	recoverAll := func() bool {
		return dispatch(c.staleClaimed(ctx)) && dispatch(c.ownPending(ctx))
	}

	// Recover anything left from a previous life before taking new work.
	if !recoverAll() {
		return ctx.Err()
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.group,
			Consumer: c.name,
			Streams:  []string{c.stream, ">"},
			Count:    c.Count,
			Block:    c.Block,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) || ctx.Err() != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Idle tick: sweep for stragglers (orphaned / retryable), then wait.
				if !recoverAll() {
					return ctx.Err()
				}
				continue
			}
			// The group vanished mid-run: Redis restarted with no persistence
			// (streams + groups gone) or the group was deleted. Recreate it at
			// "0" so commands XADDed while it was missing are still delivered -
			// otherwise XREADGROUP returns NOGROUP forever and this consumer
			// silently stops receiving work until the process restarts.
			if strings.Contains(err.Error(), "NOGROUP") {
				if e := c.ensureGroupAt(ctx, "0"); e != nil {
					log.Printf("queue %s: NOGROUP recreate failed: %v", c.stream, e)
				} else {
					log.Printf("queue %s: consumer group vanished, recreated at 0", c.stream)
				}
			} else {
				log.Printf("queue %s: read error: %v", c.stream, err)
			}
			// Back off briefly so a persistent error doesn't hot-loop.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		for _, st := range res {
			if !dispatch(st.Messages) {
				return ctx.Err()
			}
		}
	}
}

// ownPending lists this consumer's already-delivered-but-unacked entries
// (XREADGROUP id "0") for reprocessing. Dedup skips ones that actually completed.
func (c *Consumer) ownPending(ctx context.Context) []redis.XMessage {
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.name,
		Streams:  []string{c.stream, "0"},
		Count:    c.Count,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil
	}
	var out []redis.XMessage
	for _, st := range res {
		out = append(out, st.Messages...)
	}
	return out
}

// staleClaimed takes over pending entries idle longer than ClaimMinIdle that are
// owned by a dead or renamed consumer. Best-effort: a backend without
// XAUTOCLAIM (or a transient error) is logged and skipped.
func (c *Consumer) staleClaimed(ctx context.Context) []redis.XMessage {
	msgs, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   c.stream,
		Group:    c.group,
		Consumer: c.name,
		MinIdle:  c.ClaimMinIdle,
		Start:    "0",
		Count:    c.Count,
	}).Result()
	if err != nil {
		return nil
	}
	return msgs
}

// runHandler calls handler and turns a panic into an ordinary error.
//
// Without this, the package's own promise that "a poison message can't wedge the
// queue" held only for a handler that RETURNS an error. A handler that PANICS
// took the whole process with it: Run is started from a bare goroutine in both
// consumers (the node's command loop and the migration orchestrator), so nothing
// above it recovers. The message was still pending, the next start redelivered
// it through recoverPending, and it panicked again - and because the attempts
// counter only advances on a returned error, MaxDeliveries never fired and the
// dead-letter path was never reached. One malformed command wedged a node
// permanently, through every restart.
//
// Converting it to an error puts a panicking message on the normal
// retry-then-dead-letter path and keeps the stack in the log, where the ordinary
// failure logging already carries it.
func runHandler(ctx context.Context, handler Handler, data []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return handler(ctx, data)
}

// handleOne runs the dedup → handler → mark-done → ACK sequence for one message.
func (c *Consumer) handleOne(ctx context.Context, m redis.XMessage, handler Handler) {
	// Already running here: a recovery read listed an entry whose handler has
	// not returned yet. Drop it - the live run owns the ACK. Guarding here
	// covers all three delivery paths (live ">", ownPending, staleClaimed).
	if !c.beginInflight(m.ID) {
		return
	}
	defer c.endInflight(m.ID)
	c.process(ctx, m, handler)
}

// process runs one entry the caller has already claimed with beginInflight.
func (c *Consumer) process(ctx context.Context, m redis.XMessage, handler Handler) {
	// Already processed (ACK lost / redelivered): just ACK and move on.
	if n, _ := c.rdb.Exists(ctx, c.doneKey(m.ID)).Result(); n == 1 {
		c.ack(ctx, m.ID)
		return
	}

	data := msgData(m)
	if data == nil {
		// Unparseable entry — dead-letter it so it can't loop forever.
		log.Printf("queue %s: message %s has no data field, dead-lettering", c.stream, m.ID)
		c.deadLetter(ctx, m.ID, []byte("<missing data field>"))
		return
	}

	if err := runHandler(ctx, handler, data); err != nil {
		attempts, _ := c.rdb.Incr(ctx, c.attemptsKey(m.ID)).Result()
		c.rdb.Expire(ctx, c.attemptsKey(m.ID), c.DedupTTL)
		if attempts >= c.MaxDeliveries {
			log.Printf("queue %s: message %s failed %d times, dead-lettering: %v", c.stream, m.ID, attempts, err)
			c.deadLetter(ctx, m.ID, data)
			return
		}
		log.Printf("queue %s: message %s handler error (attempt %d/%d), will retry: %v", c.stream, m.ID, attempts, c.MaxDeliveries, err)
		// Leave it pending (no ACK) for redelivery.
		return
	}

	// Success: mark processed (dedup window) and ACK.
	c.rdb.Set(ctx, c.doneKey(m.ID), "1", c.DedupTTL)
	c.ack(ctx, m.ID)
	c.rdb.Del(ctx, c.attemptsKey(m.ID))
}

func (c *Consumer) ack(ctx context.Context, id string) {
	if err := c.rdb.XAck(ctx, c.stream, c.group, id).Err(); err != nil {
		log.Printf("queue %s: XACK %s failed: %v", c.stream, id, err)
	}
}

// deadLetter parks a poison/unparseable message on a side stream and ACKs the
// original so the main queue keeps flowing. Operators can inspect "<stream>:dead".
//
// The ACK is CONDITIONAL on the park succeeding. Acking regardless would mean a
// failed XAdd silently destroys the payload - the one message an operator most
// needs to look at, discarded precisely when the system is already unhealthy.
// Leaving it pending instead costs a retry on the next recovery pass and parks
// it as soon as Redis accepts the write again.
func (c *Consumer) deadLetter(ctx context.Context, id string, data []byte) {
	if err := c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: c.deadKey(),
		MaxLen: DefaultMaxLen,
		Approx: true,
		Values: map[string]interface{}{"data": data, "orig_id": id},
	}).Err(); err != nil {
		log.Printf("queue %s: could NOT park message %s on %s (%v) - leaving it pending rather than dropping it",
			c.stream, id, c.deadKey(), err)
		return
	}
	c.ack(ctx, id)
	c.rdb.Del(ctx, c.attemptsKey(id))
}

func msgData(m redis.XMessage) []byte {
	if v, ok := m.Values["data"].(string); ok {
		return []byte(v)
	}
	return nil
}
