// Package quota is the single source of truth for beam upload limits, shared by
// the node (beam gRPC upload path) and core (browser HTTP upload path) so both
// enforce the SAME admin-configured caps against the SAME per-user/day Redis
// bucket. Keeping the key format, thresholds, and read/increment logic in one
// place prevents the two paths from silently drifting apart, which would make a
// per-user quota trivially evadable by switching upload paths.
//
// Config keys (published by core SaveBeamSettings): MaxUploadBytesKey and
// DailyUploadBytesKey, both byte counts. A MISSING key means no limit and a
// present one is that cap INCLUDING 0, which forbids uploads - see the
// constants below, which are the authority on this. The per-user/day counter
// lives under DailyKey.
//
// What fails OPEN is everything that is not an answer: a nil client, a missing
// key, an unparseable value, a NEGATIVE limit, or an empty username. A cap of
// zero is an answer and is enforced.
//
// This paragraph used to say "0 = unlimited" and "a non-positive limit means no
// limit", which the code thirty lines below has not done since the limits
// convention landed. A stale doc stating the opposite of its own package is how
// the next caller reintroduces the defect the convention exists to prevent.
//
// INVARIANT for callers: the daily quota is keyed by username, and an empty
// username disables it entirely (so distinct anonymous/unauthenticated sessions
// never collide in one shared bucket). Today's enforcement paths (core HTTP
// upload, beam gRPC upload) always carry the authenticated username, so this is
// never hit. A NEW upload/write path MUST pass that authenticated username to
// CheckDailyQuota/RecordDailyUsage — forgetting it silently grants NO quota
// rather than failing closed. Deliberately not a hard deny here: guarding a path
// that does not exist would be defensive code for an impossible case.
package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// MaxUploadBytesKey / DailyUploadBytesKey are the Redis config keys core
	// publishes from BeamSettings. Each holds a byte count, and the key is
	// DELETED when the operator wants no limit.
	//
	// Missing = no limit, present = that cap INCLUDING 0, which means uploads are
	// not allowed. Both used to read 0 as "no limit", so the one number an
	// operator could type to forbid uploads was the number that switched the
	// check off - the platform limit convention (services.Limits) exists to make
	// that unrepresentable, and this is its Redis-side half.
	MaxUploadBytesKey   = "beam:max_upload_bytes"
	DailyUploadBytesKey = "beam:daily_upload_bytes"

	// dailyCounterTTL keeps a day's counter around long enough to self-clean once
	// the date rolls over (the key already carries the UTC date).
	dailyCounterTTL = 48 * time.Hour
)

// DailyKeyPrefix is every counter key belonging to one user, up to the date.
//
// It exists so the Redis ACL can name a user's counters without re-spelling the
// key format: core/services/redisacl builds a per-node grant out of this prefix,
// and a change here therefore moves the key and its permission together. Written
// the other way round - the ACL carrying its own copy of the literal - a rename
// would leave the grant pointing at a namespace nothing writes, and the quota
// package fails OPEN, so the counters would silently stop being enforced with
// nothing failing anywhere.
func DailyKeyPrefix(username string) string {
	return "dylaris:beam:daily:" + username + ":"
}

// DailyKey is the per-user, per-day upload counter key. Kept a pure function of
// (username, day) so its format is testable without Redis. The day is taken in
// UTC so the window does not shift with node/core timezone.
func DailyKey(username string, day time.Time) string {
	return fmt.Sprintf("%s%s", DailyKeyPrefix(username), day.UTC().Format("2006-01-02"))
}

// ExceedsSizeCap reports whether a single upload of `size` bytes is larger than
// `capBytes`. A nil cap means "no cap"; a cap of 0 refuses any non-empty upload.
func ExceedsSizeCap(size int64, capBytes *int64) bool {
	return capBytes != nil && size > *capBytes
}

// ExceedsDaily reports whether adding `incoming` to today's `used` would exceed
// `limit`. A nil limit means "no limit"; a limit of 0 refuses any upload.
func ExceedsDaily(used int64, limit *int64, incoming int64) bool {
	return limit != nil && used+incoming > *limit
}

// MaxUploadCap returns the configured absolute per-upload cap in bytes, or nil
// for no cap. A caller checking many items (e.g. a multi-file HTTP upload) reads
// it ONCE and applies ExceedsSizeCap per item, instead of a Redis round-trip
// each. Fail-open: nil rdb, or a missing/unparseable key, means no cap.
func MaxUploadCap(ctx context.Context, rdb *redis.Client) *int64 {
	if rdb == nil {
		return nil
	}
	capBytes, err := rdb.Get(ctx, MaxUploadBytesKey).Int64()
	if err != nil || capBytes < 0 {
		return nil
	}
	return &capBytes
}

// CheckSizeCap reports whether a single upload of `size` bytes is allowed under
// the configured absolute per-upload cap. Fail-open (see MaxUploadCap). The
// returned capBytes is the configured cap (0 when none) for the caller's error
// message. For many items in one request, read MaxUploadCap once and use
// ExceedsSizeCap instead of calling this per item.
func CheckSizeCap(ctx context.Context, rdb *redis.Client, size int64) (allowed bool, capBytes *int64) {
	capBytes = MaxUploadCap(ctx, rdb)
	return !ExceedsSizeCap(size, capBytes), capBytes
}

// DailyUsage returns the user's bytes uploaded today and the configured daily
// limit, which is nil when there is none (nil rdb, empty username, a missing or
// unparseable key). A caller that needs a remaining-quota ceiling (e.g. the
// streaming SFTP path) uses limit-used. Fail-open: a counter read error reports
// used=0.
func DailyUsage(ctx context.Context, rdb *redis.Client, username string) (used int64, limit *int64) {
	if rdb == nil || username == "" {
		return 0, nil
	}
	n, err := rdb.Get(ctx, DailyUploadBytesKey).Int64()
	if err != nil || n < 0 {
		return 0, nil
	}
	used, err = rdb.Get(ctx, DailyKey(username, time.Now())).Int64()
	if err != nil {
		used = 0 // no counter yet today
	}
	return used, &n
}

// CheckDailyQuota reports whether adding `incoming` bytes to `username`'s daily
// upload total is allowed under the configured daily limit. Fail-open (see
// DailyUsage): no limit / no rdb / no username all return allowed=true. Returns
// the current used total and the configured limit for the caller's error
// message. Best-effort: the counter is advisory (bumped on completion via
// RecordDailyUsage), so this reflects the value at call time and concurrent
// uploads by the same user are not serialized against the limit.
func CheckDailyQuota(ctx context.Context, rdb *redis.Client, username string, incoming int64) (allowed bool, used int64, limit *int64) {
	used, limit = DailyUsage(ctx, rdb, username)
	return !ExceedsDaily(used, limit, incoming), used, limit
}

// RecordDailyUsage adds `n` bytes to `username`'s daily upload counter and
// re-arms its TTL (IncrBy + Expire in one pipeline). Best-effort: a Redis error
// just means the usage is not accounted this round. A no-op when rdb/username is
// absent or n is non-positive.
func RecordDailyUsage(ctx context.Context, rdb *redis.Client, username string, n int64) {
	if rdb == nil || username == "" || n <= 0 {
		return
	}
	key := DailyKey(username, time.Now())
	pipe := rdb.Pipeline()
	pipe.IncrBy(ctx, key, n)
	pipe.Expire(ctx, key, dailyCounterTTL)
	_, _ = pipe.Exec(ctx)
}
