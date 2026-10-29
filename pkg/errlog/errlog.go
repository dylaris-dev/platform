package errlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Entry represents a single error/warning log entry written to Redis.
type Entry struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"`  // ERROR, WARN, INFO
	Source    string `json:"source"` // e.g. "gate:handler", "link:stream"
	Message   string `json:"message"`
}

// Logger writes structured log entries to a Redis Stream.
// Stream key: dylaris:errors:{service}:{instanceID}
type Logger struct {
	client     *redis.Client
	streamKey  string
	maxEntries int64
	expireWarn sync.Once
}

// How long a stream outlives its last entry.
//
// Every write refreshes it, so a component that is alive keeps its record and
// only a stream nobody writes to any more ages out - which is exactly the set
// that has no owner left to clean it.
//
// It exists because the instance id is usually a container hostname, so each
// redeploy starts a NEW stream and abandons the old one with no expiry at all.
// Measured in production on 2026-09-03: 88 error streams holding 2445 entries,
// 68 of them the hub's against two live instances. Nothing was ever going to
// remove them, and the panel's Errors screen SCANS AND READS every one on every
// load - so the cost is not the stored bytes, it is that the screen gets slower
// and noisier for as long as the platform keeps being deployed.
//
// A month is long enough that the record of a component which died last week is
// still there to look at, and short enough that the set stays bounded.
const streamTTL = 30 * 24 * time.Hour

// Services is the canonical set of service names that may appear in a stream
// key. It is the SAME list a producer picks its name from and a reader scans
// for, which is the whole point of it existing.
//
// Writing and reading used to name the service independently, and they drifted:
// the edge was renamed gate -> edge, its producer moved to "edge", and Core kept
// scanning "gate". Nothing failed - the ACL granted the new key, the writes
// landed, and the reads matched nothing - so the panel showed no edge errors,
// which is indistinguishable from an edge that has none. Every service carrying
// player traffic reported nothing for as long as that stood.
//
// CROSS-REPO: gateway/pkg/errlog carries a byte-identical copy, because the
// gateway is a separate repository and its producers (edge, link, hub, beam)
// must validate against the same list Core reads.
var Services = []string{"core", "edge", "link", "hub", "beam", "node", "warp", "voice", "bedrock"}

// IsKnownService reports whether name is one a reader will ever scan for.
func IsKnownService(name string) bool {
	for _, s := range Services {
		if s == name {
			return true
		}
	}
	return false
}

// New creates a new error logger that writes to Redis Streams.
// service: one of Services ("edge", "link", "hub", "beam", "node")
// instanceID: unique identifier for this instance (e.g. edgeID, nodeID)
//
// An unknown service name is logged rather than rejected: refusing would take a
// service down over its diagnostics, which is backwards. But it must not pass in
// silence either - a name no reader scans for makes this logger a hole that
// swallows every error written to it, and the symptom is an empty panel section
// that reads as good news.
func New(client *redis.Client, service, instanceID string) *Logger {
	if !IsKnownService(service) {
		log.Printf("errlog: service %q is not in errlog.Services %v - these entries are written but nothing reads them; add the name to BOTH copies of Services", service, Services)
	}
	return &Logger{
		client:     client,
		streamKey:  fmt.Sprintf("dylaris:errors:%s:%s", service, instanceID),
		maxEntries: 500,
	}
}

// StreamKey returns the Redis stream key used by this logger.
func (l *Logger) StreamKey() string {
	return l.streamKey
}

// Error logs an error-level entry.
func (l *Logger) Error(source, message string) {
	l.write("ERROR", source, message)
}

// Warn logs a warning-level entry.
func (l *Logger) Warn(source, message string) {
	l.write("WARN", source, message)
}

// Info logs an info-level entry.
func (l *Logger) Info(source, message string) {
	l.write("INFO", source, message)
}

// Errorf logs a formatted error-level entry.
func (l *Logger) Errorf(source, format string, args ...interface{}) {
	l.write("ERROR", source, fmt.Sprintf(format, args...))
}

// Warnf logs a formatted warning-level entry.
func (l *Logger) Warnf(source, format string, args ...interface{}) {
	l.write("WARN", source, fmt.Sprintf(format, args...))
}

func (l *Logger) write(level, source, message string) {
	if l == nil || l.client == nil {
		return
	}

	entry := Entry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     level,
		Source:    source,
		Message:   message,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("[errlog] marshal error: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// The expiry rides along with the write rather than being set once at
	// construction: XADD does not refresh a TTL, so setting it once would let a
	// busy stream expire out from under a component that is still writing.
	pipe := l.client.Pipeline()
	add := pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: l.streamKey,
		MaxLen: l.maxEntries,
		Approx: true,
		Values: map[string]interface{}{"data": string(data)},
	})
	expire := pipe.Expire(ctx, l.streamKey, streamTTL)
	// Exec reports the first failure; each command is inspected on its own
	// below, because the two failures mean different things.
	_, _ = pipe.Exec(ctx)

	if err := add.Err(); err != nil {
		// Don't recurse - just log locally
		log.Printf("[errlog] redis write failed: %v", err)
		return
	}
	if err := expire.Err(); err != nil {
		// The entry landed, so this is not a lost error - but the stream is now
		// immortal, which is the thing this was added to stop. Once per process:
		// it would otherwise repeat on every single write, and the most likely
		// cause is an ACL that omits the permission, which does not fix itself.
		l.expireWarn.Do(func() {
			log.Printf("[errlog] could not set the expiry on %s, so it will not age out: %v", l.streamKey, err)
		})
	}
}

// ReadEntries reads the latest entries from any error stream.
// Returns up to `count` entries, newest first.
func ReadEntries(client *redis.Client, streamKey string, count int64) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	results, err := client.XRevRangeN(ctx, streamKey, "+", "-", count).Result()
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(results))
	for _, msg := range results {
		dataStr, ok := msg.Values["data"].(string)
		if !ok {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(dataStr), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// ScanErrorStreams finds all error streams matching a pattern.
func ScanErrorStreams(client *redis.Client, service string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pattern := fmt.Sprintf("dylaris:errors:%s:*", service)
	var keys []string
	var cursor uint64

	for {
		batch, nextCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return keys, err
		}
		keys = append(keys, batch...)
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}
