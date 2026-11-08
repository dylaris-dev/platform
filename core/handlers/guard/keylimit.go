package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"dylaris-core/handlers/reqctx"

	"github.com/redis/go-redis/v9"
)

// Classes of the external gateway API. Numbers are the Stage 2 contract.
const (
	KeyClassRead  = "read"  // 120 per minute
	KeyClassWrite = "write" // 30 per minute
	KeyClassMint  = "mint"  // 10 per hour: kit mint, kit roll, API key roll
)

var keyClassBudget = map[string]struct {
	max    int64
	window time.Duration
}{
	KeyClassRead:  {120, time.Minute},
	KeyClassWrite: {30, time.Minute},
	KeyClassMint:  {10, time.Hour},
}

// KeyClassLimiter caps an API key per class of call. The counters live in
// Redis, so every Core replica charges the same window; the in-memory limiter
// on the key itself (api_keys.rate_per_min) still applies on top.
type KeyClassLimiter struct {
	rdb *redis.Client
	now func() time.Time

	lastLinkLog atomic.Int64
}

func NewKeyClassLimiter(rdb *redis.Client) *KeyClassLimiter {
	return &KeyClassLimiter{rdb: rdb, now: time.Now}
}

// Limit must sit INSIDE the API-key authentication wrapper: it needs the key id
// from the context, and unauthenticated requests must not be charged to a key.
//
// Fixed window, not sliding: a caller can land 2x the budget across a window
// edge. ponytail: accepted for a budget this coarse; a sliding log in Redis if
// the burst ever matters.
//
// Redis down: read passes (a listing is harmless and the key's own budget still
// applies), write and mint answer 503 because they create or change state and
// an unmetered mint is exactly what the cap exists to stop.
func (l *KeyClassLimiter) Limit(class string, next http.HandlerFunc) http.HandlerFunc {
	b, known := keyClassBudget[class]
	if !known {
		panic("guard: unknown key limit class " + class)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		key := reqctx.APIKeyFromContext(r)
		if key == nil {
			// Wired outside the auth wrapper: refuse rather than skip the limit.
			writeKeyLimitError(w, http.StatusUnauthorized, "Authentication required", "unauthorized")
			return
		}
		now := l.now()
		secs := int64(b.window / time.Second)
		slot := now.Unix() / secs
		rkey := "dylaris:ratelimit:key:" + strconv.Itoa(key.ID) + ":" + class + ":" + strconv.FormatInt(slot, 10)

		n, err := l.charge(r.Context(), rkey, b.window)
		if err != nil {
			if class == KeyClassRead {
				next(w, r)
				return
			}
			writeKeyLimitError(w, http.StatusServiceUnavailable, "Rate limiting is unavailable right now. Try again shortly.", "unavailable")
			return
		}
		if n > b.max {
			wait := (slot+1)*secs - now.Unix()
			if wait < 1 {
				wait = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(wait, 10))
			writeKeyLimitError(w, http.StatusTooManyRequests, fmt.Sprintf("Too many requests for this key. Try again in %d seconds.", wait), "rate_limited")
			return
		}
		next(w, r)
	}
}

// Same shape as the external gateway's own error answers.
func writeKeyLimitError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": msg, "code": code})
}

// charge counts one call in a fixed-window Redis counter.
func (l *KeyClassLimiter) charge(parent context.Context, rkey string, window time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	n, err := l.rdb.Incr(ctx, rkey).Result()
	if err == nil && n == 1 {
		// A crash between the two calls leaves a counter without TTL; the slot
		// is part of the key so it is dead weight, never a stuck limit.
		err = l.rdb.Expire(ctx, rkey, window+10*time.Second).Err()
	}
	return n, err
}

// LinkMaxPerMinute is what one link key may spend on /api/warp/link/*. A link
// needs ~45 calls a minute; the rest is headroom for restarts and retries.
const LinkMaxPerMinute = 300

// LimitLink caps a warp link key at LinkMaxPerMinute, shared by every Core
// replica. keyID must come from the warp-key middleware, so it sits INSIDE it:
// unauthenticated requests are never charged to a key, and the counter is the
// key's row id, never the presented secret.
//
// Fails OPEN on a Redis error: a heartbeat that stops because Redis blipped
// drops online_link and with it the players' routing. The per-IP guard in
// front still applies. 429 is a plain non-2xx for the link client (only 401 and
// 403 mean "key refused"), so it just retries on its next tick.
func (l *KeyClassLimiter) LimitLink(keyID func(*http.Request) (int, bool), next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := keyID(r)
		if !ok {
			writeKeyLimitError(w, http.StatusUnauthorized, "Authentication required", "unauthorized")
			return
		}
		now := l.now()
		minute := now.Unix() / 60
		n, err := l.charge(r.Context(), "dylaris:ratelimit:link:"+strconv.Itoa(id)+":"+strconv.FormatInt(minute, 10), time.Minute)
		if err != nil {
			l.logLinkFailure(minute, err)
			next(w, r)
			return
		}
		if n > LinkMaxPerMinute {
			wait := (minute+1)*60 - now.Unix()
			if wait < 1 {
				wait = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(wait, 10))
			writeKeyLimitError(w, http.StatusTooManyRequests, fmt.Sprintf("Too many requests for this link key. Try again in %d seconds.", wait), "rate_limited")
			return
		}
		next(w, r)
	}
}

// logLinkFailure logs a Redis failure at most once a minute: it would otherwise
// fire on every call of every link while Redis is away.
func (l *KeyClassLimiter) logLinkFailure(minute int64, err error) {
	if l.lastLinkLog.Swap(minute) != minute {
		log.Printf("link rate limit: redis unavailable, failing open: %v", err)
	}
}
