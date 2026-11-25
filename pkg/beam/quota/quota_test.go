package quota

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func cap64(v int64) *int64 { return &v }

func TestExceedsSizeCap(t *testing.T) {
	cases := []struct {
		name     string
		size     int64
		capBytes *int64
		want     bool
	}{
		// nil is the only "no cap" now. A zero cap used to mean unlimited, which
		// made the one value an operator could type to forbid uploads the value
		// that turned the check off.
		{"no cap", 999999, nil, false},
		{"a cap of NONE refuses any non-empty upload", 1, cap64(0), true},
		{"a cap of NONE still allows an empty one", 0, cap64(0), false},
		{"under the cap", 100, cap64(200), false},
		{"exactly at the cap", 200, cap64(200), false},
		{"one byte over", 201, cap64(200), true},
		{"empty upload under cap", 0, cap64(200), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExceedsSizeCap(c.size, c.capBytes); got != c.want {
				t.Errorf("ExceedsSizeCap(%d, %v) = %v, want %v", c.size, c.capBytes, got, c.want)
			}
		})
	}
}

func TestExceedsDaily(t *testing.T) {
	cases := []struct {
		name     string
		used     int64
		limit    *int64
		incoming int64
		want     bool
	}{
		{"no limit", 100, nil, 999999, false},
		{"a limit of NONE refuses any upload", 0, cap64(0), 1, true},
		{"a limit of NONE allows an empty one", 0, cap64(0), 0, false},
		{"fits exactly at limit", 100, cap64(200), 100, false},
		{"one byte over", 100, cap64(200), 101, true},
		{"already at limit, empty upload", 200, cap64(200), 0, false},
		{"already over", 300, cap64(200), 0, true},
		{"comfortable headroom", 50, cap64(1000), 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExceedsDaily(c.used, c.limit, c.incoming); got != c.want {
				t.Errorf("ExceedsDaily(%d, %v, %d) = %v, want %v", c.used, c.limit, c.incoming, got, c.want)
			}
		})
	}
}

// TestDailyKey pins the counter key format and, critically, that the day is
// normalized to UTC so node and core (possibly in different timezones) share the
// same window.
func TestDailyKey(t *testing.T) {
	cases := []struct {
		name     string
		username string
		day      time.Time
		want     string
	}{
		{
			name:     "utc midday",
			username: "alice",
			day:      time.Date(2026, 7, 21, 15, 4, 5, 0, time.UTC),
			want:     "dylaris:beam:daily:alice:2026-07-21",
		},
		{
			name:     "positive offset rolls back to prior utc day",
			username: "bob",
			day:      time.Date(2026, 7, 22, 0, 30, 0, 0, time.FixedZone("X", 2*3600)),
			want:     "dylaris:beam:daily:bob:2026-07-21",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DailyKey(BucketFor("", c.username), c.day); got != c.want {
				t.Errorf("DailyKey(%q, %v) = %q, want %q", c.username, c.day, got, c.want)
			}
		})
	}
}

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func TestMaxUploadCap(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)

	// A MISSING key is the no-cap answer now. Core deletes it rather than
	// writing 0, so "no limit" and "a limit of none" stay distinguishable all
	// the way to the node.
	if got := MaxUploadCap(ctx, rdb); got != nil {
		t.Errorf("no cap set: got %d, want nil", *got)
	}
	mr.Set(MaxUploadBytesKey, "4096")
	if got := MaxUploadCap(ctx, rdb); got == nil || *got != 4096 {
		t.Errorf("cap set: got %v, want 4096", got)
	}
	mr.Set(MaxUploadBytesKey, "0")
	if got := MaxUploadCap(ctx, rdb); got == nil || *got != 0 {
		t.Errorf("a stored 0 must survive as a cap of NONE, got %v", got)
	}
	mr.Set(MaxUploadBytesKey, "not-a-number")
	if got := MaxUploadCap(ctx, rdb); got != nil {
		t.Errorf("unparseable cap: got %d, want nil", *got)
	}
	if got := MaxUploadCap(ctx, nil); got != nil {
		t.Errorf("nil rdb: got %d, want nil", *got)
	}
}

func TestCheckSizeCap(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)

	if ok, _ := CheckSizeCap(ctx, rdb, 1<<30); !ok {
		t.Error("no cap configured should allow")
	}
	mr.Set(MaxUploadBytesKey, "1000")
	if ok, capB := CheckSizeCap(ctx, rdb, 1001); ok || capB == nil || *capB != 1000 {
		t.Errorf("over cap: ok=%v cap=%v, want ok=false cap=1000", ok, capB)
	}
	if ok, _ := CheckSizeCap(ctx, rdb, 1000); !ok {
		t.Error("exactly at cap should allow")
	}
	if ok, _ := CheckSizeCap(ctx, nil, 1<<40); !ok {
		t.Error("nil rdb should allow")
	}
}

func TestCheckDailyQuota(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)

	if ok, _, _ := CheckDailyQuota(ctx, rdb, alice, 1<<30); !ok {
		t.Error("no limit configured should allow")
	}
	mr.Set(DailyUploadBytesKey, "1000")
	if ok, used, limit := CheckDailyQuota(ctx, rdb, alice, 500); !ok || used != 0 || limit == nil || *limit != 1000 {
		t.Errorf("no counter yet + under limit: ok=%v used=%d limit=%v, want ok=true used=0 limit=1000", ok, used, limit)
	}
	mr.Set(DailyKey(alice, time.Now()), "800")
	if ok, used, _ := CheckDailyQuota(ctx, rdb, alice, 300); ok || used != 800 {
		t.Errorf("used 800 + incoming 300 vs limit 1000: ok=%v used=%d, want ok=false used=800", ok, used)
	}
	if ok, _, _ := CheckDailyQuota(ctx, rdb, "", 1<<30); !ok {
		t.Error("empty username should allow (never a shared bucket)")
	}
	if ok, _, _ := CheckDailyQuota(ctx, nil, alice, 1<<30); !ok {
		t.Error("nil rdb should allow")
	}
}

func TestDailyUsage(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)

	isLimit := func(l *int64, want int64) bool { return l != nil && *l == want }

	if used, limit := DailyUsage(ctx, rdb, alice); used != 0 || limit != nil {
		t.Errorf("no limit set: (%d, %v), want (0, nil)", used, limit)
	}
	mr.Set(DailyUploadBytesKey, "1000")
	if used, limit := DailyUsage(ctx, rdb, alice); used != 0 || !isLimit(limit, 1000) {
		t.Errorf("limit set, no counter: (%d, %v), want (0, 1000)", used, limit)
	}
	mr.Set(DailyKey(alice, time.Now()), "750")
	if used, limit := DailyUsage(ctx, rdb, alice); used != 750 || !isLimit(limit, 1000) {
		t.Errorf("counter set: (%d, %v), want (750, 1000)", used, limit)
	}
	// A stored 0 is a limit of NONE and must reach the caller as one.
	mr.Set(DailyUploadBytesKey, "0")
	if _, limit := DailyUsage(ctx, rdb, alice); !isLimit(limit, 0) {
		t.Errorf("a stored 0 came back as %v, want a limit of 0", limit)
	}
	if used, limit := DailyUsage(ctx, rdb, ""); used != 0 || limit != nil {
		t.Errorf("empty username: (%d, %v), want (0, nil)", used, limit)
	}
	if used, limit := DailyUsage(ctx, nil, alice); used != 0 || limit != nil {
		t.Errorf("nil rdb: (%d, %v), want (0, nil)", used, limit)
	}
}

func TestRecordDailyUsage(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	key := DailyKey(bob, time.Now())

	RecordDailyUsage(ctx, rdb, bob, 100)
	RecordDailyUsage(ctx, rdb, bob, 50)
	if got, err := rdb.Get(ctx, key).Int64(); err != nil || got != 150 {
		t.Fatalf("counter = %d (err %v), want 150", got, err)
	}
	if ttl := mr.TTL(key); ttl <= 0 {
		t.Errorf("TTL not armed: %v", ttl)
	}

	// No-ops must not touch the counter.
	RecordDailyUsage(ctx, rdb, "", 100)  // empty username
	RecordDailyUsage(ctx, nil, bob, 100) // nil rdb
	RecordDailyUsage(ctx, rdb, bob, 0)   // non-positive
	if got, _ := rdb.Get(ctx, key).Int64(); got != 150 {
		t.Errorf("a no-op changed the counter to %d, want 150", got)
	}
}

var alice, bob = BucketFor("", "alice"), BucketFor("", "bob")

const aliceID = "3f1c2a9e-0b7d-4e21-9c5a-6d8e7f901234"

// TestBucketFor pins which identity an upload is booked to. The id wins
// whenever it is a real one, so a rename cannot start a fresh day; the name is
// only the fallback for a caller holding no id (a ticket or SFTP list from a
// Core older than the id).
func TestBucketFor(t *testing.T) {
	cases := []struct {
		name, id, username string
		want               Bucket
	}{
		{"id wins over the name", aliceID, "alice", "dylaris:beam:daily-id:" + aliceID + ":"},
		{"a renamed account keeps its bucket", aliceID, "alice2", "dylaris:beam:daily-id:" + aliceID + ":"},
		{"no id falls back to the name", "", "alice", "dylaris:beam:daily:alice:"},
		{"a malformed id is not interpolated", "x:* ~*", "alice", "dylaris:beam:daily:alice:"},
		{"neither is no bucket", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BucketFor(c.id, c.username); got != c.want {
				t.Errorf("BucketFor(%q, %q) = %q, want %q", c.id, c.username, got, c.want)
			}
		})
	}
	// "id" is a valid username, and its name grant must not cover id counters.
	idKey := DailyKey(BucketFor(aliceID, "alice"), time.Now())
	if strings.HasPrefix(idKey, DailyKeyPrefix("id")) || strings.HasPrefix(idKey, DailyKeyPrefix("daily-id")) {
		t.Errorf("a username's prefix covers the id counter %s", idKey)
	}
}

// TestRenameDoesNotResetTheDay is the R34-09 scenario at the package level:
// bytes booked under one name are still there under the next.
func TestRenameDoesNotResetTheDay(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	mr.Set(DailyUploadBytesKey, "1000")
	RecordDailyUsage(ctx, rdb, BucketFor(aliceID, "alice"), 1000)
	if ok, used, _ := CheckDailyQuota(ctx, rdb, BucketFor(aliceID, "alice2"), 1); ok || used != 1000 {
		t.Errorf("after a rename: ok=%v used=%d, want the day's 1000 still counted", ok, used)
	}
}

// A counter that cannot be read or written is no longer silent: it reads as
// nothing used (see DailyUsage for why it stays open), but leaves a log line.
// The NOPERM of a deliberately ungranted counter stays quiet.
func TestCounterErrorsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cases := []struct {
		name   string
		err    error
		logged bool
	}{
		{"wrong type", errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"), true},
		{"no counter yet", redis.Nil, false},
		{"ungranted counter", errors.New("NOPERM No permissions to access a key"), false},
		{"no error", nil, false},
	}
	for _, c := range cases {
		buf.Reset()
		logCounterError("reading", alice, c.err)
		if got := buf.Len() > 0; got != c.logged {
			t.Errorf("%s: logged=%v, want %v (%q)", c.name, got, c.logged, buf.String())
		}
	}

	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	mr.Set(DailyUploadBytesKey, "1000")
	mr.HSet(DailyKey(alice, time.Now()), "f", "v") // a hash: GET answers WRONGTYPE
	buf.Reset()
	if used, _ := DailyUsage(ctx, rdb, alice); used != 0 || !strings.Contains(buf.String(), "reading") {
		t.Errorf("unreadable counter: used=%d log=%q, want 0 and a log line", used, buf.String())
	}
	buf.Reset()
	RecordDailyUsage(ctx, rdb, alice, 5)
	if !strings.Contains(buf.String(), "booking") {
		t.Errorf("a failed booking left no log line: %q", buf.String())
	}
}
