package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/handlers/health"
	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
)

func shortStreams(t *testing.T) {
	t.Helper()
	t.Cleanup(httpx.SetSSEMaxStreamAge(300 * time.Millisecond))
}

// A stream used to be checked once, when it opened, and then run until the
// client left. It now ends on its own, so the reconnect is checked again - and
// the console picks up after the last line it delivered instead of dropping
// whatever arrived in between.
func TestConsoleStreamEndsAndResumesAfterTheLastLine(t *testing.T) {
	shortStreams(t)
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", ActiveSubServer: "main"}}
	rdb := handlertest.RedisClient(t)
	h := &ConsoleHandler{d: Deps{Store: fs, Redis: rdb}}
	key := consoleStreamKey(fs.server, "")
	ctx := context.Background()
	first, _ := rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"line": "one"}}).Result()
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"line": "two"}})

	req := httptest.NewRequest("GET", "/api/servers/1/console/stream", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "1"})
	req.Header.Set("Last-Event-ID", first)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { h.StreamConsole(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the console stream did not end on its own")
	}
	body := rec.Body.String()
	if strings.Contains(body, "data: one") || !strings.Contains(body, "data: two") {
		t.Fatalf("resumed stream = %q, want only the line after the last one delivered", body)
	}
	if !strings.Contains(body, "id: ") {
		t.Fatal("lines go out without the id a reconnect resumes from")
	}
}

// Every stream opens with the retry line, so the rotation reconnects after a
// second instead of the browser's default.
func TestStreamsOpenWithTheRetryLine(t *testing.T) {
	shortStreams(t)
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", ActiveSubServer: "main"}}
	rdb := handlertest.RedisClient(t)
	run := func(name string, serve func(*httptest.ResponseRecorder)) string {
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { serve(rec); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s stream did not end on its own", name)
		}
		return rec.Body.String()
	}
	req := func(path string) *http.Request {
		r := httptest.NewRequest("GET", path, nil)
		return mux.SetURLVars(r, map[string]string{"id": "1"})
	}
	console := &ConsoleHandler{d: Deps{Store: fs, Redis: rdb}}
	if body := run("console", func(rec *httptest.ResponseRecorder) { console.StreamConsole(rec, req("/api/servers/1/console/stream")) }); !strings.HasPrefix(body, httpx.SSERetryLine) {
		t.Errorf("console stream = %q, want it to open with %q", body, httpx.SSERetryLine)
	}
	events := health.NewSystemEventsHandler(health.Deps{Store: fs, Redis: rdb})
	if body := run("events", func(rec *httptest.ResponseRecorder) { events.StreamEvents(rec, req("/api/system/events")) }); !strings.Contains(body, httpx.SSERetryLine) {
		t.Errorf("system events stream = %q, want %q", body, httpx.SSERetryLine)
	}
}
