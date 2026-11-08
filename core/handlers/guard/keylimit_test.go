package guard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func keyLimitSetup(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb
}

func keyCall(h http.HandlerFunc, keyID int) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/x", nil)
	r = r.WithContext(reqctx.WithAPIKey(r.Context(), &models.APIKey{ID: keyID}, false))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func TestKeyClassLimiterIsSharedThroughRedis(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	// Two limiters on one Redis stand in for two Core replicas.
	a := NewKeyClassLimiter(rdb).Limit(KeyClassWrite, ok)
	b := NewKeyClassLimiter(rdb).Limit(KeyClassWrite, ok)
	for i := 0; i < 30; i++ {
		h := a
		if i%2 == 1 {
			h = b
		}
		if w := keyCall(h, 1); w.Code != http.StatusNoContent {
			t.Fatalf("call %d = %d, want 204", i+1, w.Code)
		}
	}
	w := keyCall(a, 1)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("31st call = %d, want 429", w.Code)
	}
	var body struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Success || body.Code != "rate_limited" || !strings.HasPrefix(body.Error, "Too many requests for this key. Try again in ") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if w := keyCall(b, 1); w.Code != http.StatusTooManyRequests {
		t.Fatalf("other replica after the cap = %d, want 429", w.Code)
	}
}

func TestKeyClassLimiterSeparatesClassesAndKeys(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	l := NewKeyClassLimiter(rdb)
	write, read, mint := l.Limit(KeyClassWrite, ok), l.Limit(KeyClassRead, ok), l.Limit(KeyClassMint, ok)
	for i := 0; i < 10; i++ {
		keyCall(mint, 1)
	}
	if w := keyCall(mint, 1); w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th mint = %d, want 429", w.Code)
	}
	if w := keyCall(mint, 2); w.Code != http.StatusNoContent {
		t.Fatalf("mint on another key = %d, want 204", w.Code)
	}
	if w := keyCall(write, 1); w.Code != http.StatusNoContent {
		t.Fatalf("write after mint exhausted = %d, want 204", w.Code)
	}
	for i := 0; i < 120; i++ {
		if w := keyCall(read, 1); w.Code != http.StatusNoContent {
			t.Fatalf("read %d = %d", i+1, w.Code)
		}
	}
	if w := keyCall(read, 1); w.Code != http.StatusTooManyRequests {
		t.Fatalf("121st read = %d, want 429", w.Code)
	}
}

func TestKeyClassLimiterRetryAfter(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	l := NewKeyClassLimiter(rdb)
	l.now = func() time.Time { return time.Unix(1_700_000_040+45, 0) } // 45 s into a minute
	h := l.Limit(KeyClassWrite, ok)
	for i := 0; i < 30; i++ {
		keyCall(h, 1)
	}
	w := keyCall(h, 1)
	if w.Code != 429 {
		t.Fatalf("code = %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "15" {
		t.Fatalf("Retry-After = %q, want 15", got)
	}
	if n, _ := strconv.Atoi(w.Header().Get("Retry-After")); n < 1 {
		t.Fatal("Retry-After must be at least 1")
	}
	// Next window starts fresh.
	l.now = func() time.Time { return time.Unix(1_700_000_040+61, 0) }
	if w := keyCall(h, 1); w.Code != http.StatusNoContent {
		t.Fatalf("next window = %d, want 204", w.Code)
	}
}

func TestKeyClassLimiterRedisDown(t *testing.T) {
	mr, rdb := keyLimitSetup(t)
	l := NewKeyClassLimiter(rdb)
	mr.Close()
	if w := keyCall(l.Limit(KeyClassRead, ok), 1); w.Code != http.StatusNoContent {
		t.Fatalf("read with Redis down = %d, want 204 (fail open)", w.Code)
	}
	for _, c := range []string{KeyClassWrite, KeyClassMint} {
		w := keyCall(l.Limit(c, ok), 1)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s with Redis down = %d, want 503", c, w.Code)
		}
		if got := w.Body.String(); got == "" || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s body/content-type = %q / %q", c, got, w.Header().Get("Content-Type"))
		}
	}
}
