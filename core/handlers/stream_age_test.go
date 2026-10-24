package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
)

func shortStreams(t *testing.T) {
	t.Helper()
	prev := sseMaxStreamAge
	sseMaxStreamAge = 300 * time.Millisecond
	t.Cleanup(func() { sseMaxStreamAge = prev })
}

// A stream used to be checked once, when it opened, and then run until the
// client left. It now ends on its own, so the reconnect is checked again - and
// the console picks up after the last line it delivered instead of dropping
// whatever arrived in between.
func TestConsoleStreamEndsAndResumesAfterTheLastLine(t *testing.T) {
	shortStreams(t)
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", ActiveSubServer: "main"}}
	rdb := newServerPowerRedis(t)
	h := &ConsoleHandler{state: &AppState{Store: fs, Redis: rdb}}
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

// The ticket slides forward when a stream opens, not while it runs. A stream
// that lived as long as the ticket reconnected with one that had just expired,
// and the console went dead until a reload.
func TestStreamsEndBeforeTheirTicket(t *testing.T) {
	if sseMaxStreamAge >= sseTicketTTL {
		t.Fatalf("sseMaxStreamAge %v must be shorter than sseTicketTTL %v, or the reconnect carries an expired ticket", sseMaxStreamAge, sseTicketTTL)
	}
	// Room for the reconnect itself: the browser waits the retry interval,
	// then the request has to arrive.
	if sseTicketTTL-sseMaxStreamAge < 30*time.Second {
		t.Fatalf("only %v between stream end and ticket expiry", sseTicketTTL-sseMaxStreamAge)
	}
	if sseRetryLine != "retry: 1000\n\n" {
		t.Fatalf("sseRetryLine = %q", sseRetryLine)
	}
}

// Every stream opens with the retry line, so the rotation reconnects after a
// second instead of the browser's default.
func TestStreamsOpenWithTheRetryLine(t *testing.T) {
	shortStreams(t)
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", ActiveSubServer: "main"}}
	rdb := newServerPowerRedis(t)
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
	console := &ConsoleHandler{state: &AppState{Store: fs, Redis: rdb}}
	if body := run("console", func(rec *httptest.ResponseRecorder) { console.StreamConsole(rec, req("/api/servers/1/console/stream")) }); !strings.HasPrefix(body, sseRetryLine) {
		t.Errorf("console stream = %q, want it to open with %q", body, sseRetryLine)
	}
	events := NewSystemEventsHandler(&AppState{Store: fs, Redis: rdb})
	if body := run("events", func(rec *httptest.ResponseRecorder) { events.StreamEvents(rec, req("/api/system/events")) }); !strings.Contains(body, sseRetryLine) {
		t.Errorf("system events stream = %q, want %q", body, sseRetryLine)
	}
}
