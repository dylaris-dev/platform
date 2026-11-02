package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers/guard"
	"dylaris-core/services/demo"
	"dylaris-core/store"

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
	h := NewConsoleHandler(&AppState{Store: consoleHistoryFakeStore{}, Redis: rdb})

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
	h := NewConsoleHandler(&AppState{Store: consoleHistoryFakeStore{}, Redis: sharedClient, ConsoleRedis: consoleClient})

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

// A download whose browser went away stops: the write error returns, and the
// deferred CleanupRequest cancels the node's transfer. Ignoring it read the
// rest of the file from the node only to drop it - for a world, gigabytes.
// Pinned in the source because the handlers need a live node registry to run.
// Not the tab proxy: a 304 or HEAD answer refuses every body write by design,
// and its node side is bounded by a 30s client timeout anyway.
func TestDownloadsStopWhenTheBrowserLeaves(t *testing.T) {
	for file, want := range map[string]int{"file.go": 2} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(b), "\r\n", "\n")
		if n := strings.Count(s, "if _, err := w.Write(chunk.Data); err != nil {\n"); n != want {
			t.Errorf("%s: %d chunk writes stop on a write error, want %d", file, n, want)
		}
		if strings.Contains(s, "\tw.Write(chunk.Data)\n") {
			t.Errorf("%s: a chunk write ignores its error again", file)
		}
	}
}

type demoSettingStore struct {
	store.Store
	demo string
}

func (d demoSettingStore) GetSetting(key string) (string, error) {
	if key == demo.AccountUUIDSetting {
		return d.demo, nil
	}
	return "", nil
}

// Every demo visitor is the same account; counted per account, a handful of
// demo tabs stopped every other demo visitor's live updates.
func TestDemoVisitorsHaveTheirOwnSlots(t *testing.T) {
	st := demoSettingStore{demo: "demo-uuid"}
	req := func(user, addr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/system/events", nil)
		r.RemoteAddr = addr
		return r.WithContext(context.WithValue(r.Context(), "userID", user))
	}
	var held []func()
	defer func() {
		for _, rel := range held {
			rel()
		}
	}()
	take := func(user, addr string) bool {
		rel, ok := guard.AcquireLiveStream(httptest.NewRecorder(), req(user, addr), st, "demo-test", 1)
		if ok {
			held = append(held, rel)
		}
		return ok
	}
	if !take("demo-uuid", "203.0.113.1:1") || !take("demo-uuid", "203.0.113.2:1") {
		t.Fatal("a second demo visitor shared the first one's slot")
	}
	if take("demo-uuid", "203.0.113.1:2") {
		t.Fatal("one demo visitor got past the cap")
	}
	if !take("real-user", "203.0.113.1:3") || take("real-user", "203.0.113.9:1") {
		t.Fatal("an ordinary account is not capped per account")
	}
}
