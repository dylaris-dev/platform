package mods

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers"
	"dylaris-core/services"
)

// A 429 from Modrinth is not passed through as a 429 page, is not cached, and
// stops further calls until the reset - while cached answers keep working.
func TestModrinthProxyBacksOffOn429(t *testing.T) {
	var calls atomic.Int32
	var limited atomic.Bool
	var gotUA atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotUA.Store(r.UserAgent())
		if limited.Load() {
			w.Header().Set("X-Ratelimit-Reset", "20")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	mr := miniredis.RunT(t)
	h := NewModrinthHandler(&handlers.AppState{Cache: services.NewCache(redis.NewClient(&redis.Options{Addr: mr.Addr()}))})
	ctx := context.Background()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.proxyJSON(ctx, time.Minute, up.URL+path, rec)
		return rec
	}

	if rec := get("/cached"); rec.Code != 200 {
		t.Fatalf("warm-up = %d", rec.Code)
	}
	if ua, _ := gotUA.Load().(string); !strings.HasPrefix(ua, "Dylaris/") || !strings.Contains(ua, "https://dylaris.com") {
		t.Errorf("User-Agent = %q", ua)
	}
	limited.Store(true)
	rec := get("/other")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 answer = %d Retry-After %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	before := calls.Load()
	if rec := get("/other"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("during cooldown = %d, want 503", rec.Code)
	}
	if calls.Load() != before {
		t.Error("Modrinth was called during the cooldown")
	}
	if rec := get("/cached"); rec.Code != 200 {
		t.Errorf("cached answer during cooldown = %d, want 200", rec.Code)
	}
	limited.Store(false)
	h.cool = handlers.UpstreamCooldown{}
	if rec := get("/other"); rec.Code != 200 {
		t.Errorf("after the cooldown = %d; the 429 must not have been cached", rec.Code)
	}
}
