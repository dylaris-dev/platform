package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers/guard"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// Over the cap a stream is refused before any stream header goes out; the
// panel opens a fresh one on its own backoff.
func TestStreamConsoleRefusedOverTheCap(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	h := NewConsoleHandler(Deps{Store: consoleHistoryFakeStore{}, Redis: rdb})

	var held []func()
	for range guard.ConsoleStreamsPerUser {
		// through the guard's own entry point: the console handler and this
		// loop must count against the ONE slot table
		capped := httptest.NewRequest(http.MethodGet, "/", nil)
		capped = capped.WithContext(context.WithValue(capped.Context(), "userID", "capped-user"))
		rel, ok := guard.AcquireLiveStream(httptest.NewRecorder(), capped, nil, "console", guard.ConsoleStreamsPerUser)
		if !ok {
			t.Fatal("setup could not take the slots")
		}
		held = append(held, rel)
	}
	defer func() {
		for _, rel := range held {
			rel()
		}
	}()
	// Bounded, so a handler that streams anyway fails here instead of hanging.
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), "userID", "capped-user"), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/servers/7/console/stream", nil).WithContext(ctx)
	req = mux.SetURLVars(req, map[string]string{"id": "7"})
	rw := httptest.NewRecorder()
	h.StreamConsole(rw, req)
	if rw.Code != http.StatusTooManyRequests || strings.Contains(rw.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("code %d, content type %q", rw.Code, rw.Header().Get("Content-Type"))
	}
}

// The blocking console reads run on their own client, so open consoles cannot
// starve the shared pool every other Redis command of the replica uses.
func TestStreamConsoleReadsOnTheConsoleClient(t *testing.T) {
	shared := miniredis.RunT(t)
	console := miniredis.RunT(t)
	sharedClient := redis.NewClient(&redis.Options{Addr: shared.Addr()})
	consoleClient := redis.NewClient(&redis.Options{Addr: console.Addr()})
	defer sharedClient.Close()
	defer consoleClient.Close()
	seedConsole(t, consoleClient, 3)
	h := NewConsoleHandler(Deps{Store: consoleHistoryFakeStore{}, Redis: sharedClient, ConsoleRedis: consoleClient})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/servers/7/console/stream?after=1-0", nil).WithContext(ctx)
	req = mux.SetURLVars(req, map[string]string{"id": "7"})
	rw := httptest.NewRecorder()
	h.StreamConsole(rw, req)
	if !strings.Contains(rw.Body.String(), "id: 3-0\n") {
		t.Fatalf("lines not read from the console client: %q", rw.Body.String())
	}
}
