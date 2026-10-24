package stats

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/store"
)

// The live graph was fed the whole buffer on every connect. With streams
// ending on a timer that would redraw it every few minutes.
func TestStatsStreamReplaysTheBufferOnlyOnTheFirstConnect(t *testing.T) {
	shortStreams(t)
	fs := &statsFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid"}}
	rdb := newServerPowerRedis(t)
	h := &StatsHandler{state: &handlers.AppState{Store: fs, Redis: rdb}}
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

// The stats half of handlers' TestStreamsOpenWithTheRetryLine: the stream opens
// with the retry line, so the rotation reconnects after a second.
func TestStatsStreamOpensWithTheRetryLine(t *testing.T) {
	shortStreams(t)
	fs := &statsFakeStore{server: &models.Server{ID: 1, UUID: "srv-uuid", ActiveSubServer: "main"}}
	rdb := newServerPowerRedis(t)
	h := &StatsHandler{state: &handlers.AppState{Store: fs, Redis: rdb}}
	req := mux.SetURLVars(httptest.NewRequest("GET", "/api/servers/1/stats/stream", nil), map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.StreamStats(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stats stream did not end on its own")
	}
	if body := rec.Body.String(); !strings.HasPrefix(body, handlers.SSERetryLine) {
		t.Errorf("stats stream = %q, want it to open with %q", body, handlers.SSERetryLine)
	}
}

// statsFakeStore answers what StreamStats asks of the store: the server, and the
// live-stream limit setting (unset, so the default applies).
type statsFakeStore struct {
	store.Store
	server *models.Server
}

func (f *statsFakeStore) GetServerByID(int) (*models.Server, error) { return f.server, nil }
func (f *statsFakeStore) GetSetting(string) (string, error)         { return "", nil }

// shortStreams ends SSE streams after 300 ms; see handlers/stream_age_test.go.
func shortStreams(t *testing.T) {
	t.Helper()
	t.Cleanup(handlers.SetSSEMaxStreamAge(300 * time.Millisecond))
}

// newServerPowerRedis returns a real client backed by miniredis.
func newServerPowerRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}
