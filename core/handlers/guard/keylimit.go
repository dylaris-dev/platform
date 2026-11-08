package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		n, err := l.rdb.Incr(ctx, rkey).Result()
		if err == nil && n == 1 {
			// A crash between the two calls leaves a counter without TTL; the slot
			// is part of the key so it is dead weight, never a stuck limit.
			err = l.rdb.Expire(ctx, rkey, b.window+10*time.Second).Err()
		}
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
