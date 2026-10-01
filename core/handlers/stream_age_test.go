package handlers

import (
	"context"
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

// The live graph was fed the whole buffer on every connect. With streams
// ending on a timer that would redraw it every few minutes.
func TestStatsStreamReplaysTheBufferOnlyOnTheFirstConnect(t *testing.T) {
	shortStreams(t)
	fs := &serverPowerFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid"}}
	rdb := newServerPowerRedis(t)
	h := &StatsHandler{state: &AppState{Store: fs, Redis: rdb}}
	rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: "dylaris:server:srv-uuid:stats:buffer", Values: map[string]any{"data": `{"cpu":1}`}})

	run := func(lastID string) string {
		req := httptest.NewRequest("GET", "/api/servers/1/stats/stream", nil)
		req = mux.SetURLVars(req, map[string]string{"id": "1"})
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { h.StreamStats(rec, req); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the stats stream did not end on its own")
		}
		return rec.Body.String()
	}
	if body := run(""); !strings.Contains(body, `{"cpu":1}`) {
		t.Fatalf("first connect = %q, want the buffer", body)
	}
	if body := run("live"); strings.Contains(body, `{"cpu":1}`) {
		t.Fatalf("reconnect = %q, replayed the buffer", body)
	}
}
