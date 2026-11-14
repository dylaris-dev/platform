package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// remotePlayersStore: the caller's server list and the local remote_routes.
type remotePlayersStore struct {
	store.Store
	listed    []models.Server
	routes    []store.RemoteRoute
	routesErr error
}

func (f *remotePlayersStore) ListServersForUser(string, bool) ([]models.Server, error) {
	return f.listed, nil
}

func (f *remotePlayersStore) ListRemoteRoutes() ([]store.RemoteRoute, error) {
	return f.routes, f.routesErr
}

func playersBody(t *testing.T, h *ServerHandler) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/servers/players", nil)
	req = req.WithContext(context.WithValue(req.Context(), "userID", "u1"))
	rec := httptest.NewRecorder()
	h.GetGatewayPlayers(rec, req)
	return rec.Code, rec.Body.String()
}

func playersCalls(f *remotegwtest.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if c == "GET /players" {
			n++
		}
	}
	return n
}

const (
	uuidMine   = "0b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f"
	uuidQuiet  = "1b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f"
	uuidTheirs = "2b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f"
)

// newRemotePlayersHandler: server 12 (two addresses) and server 13 (none
// with players) are the caller's; server 14 is someone else's.
func newRemotePlayersHandler(t *testing.T, remote, known bool) (*ServerHandler, *remotegwtest.Fake, *redis.Client) {
	t.Helper()
	fake := remotegwtest.NewFake(t)
	api := remotegw.New(fake.URL(), fake.Key)
	fs := &remotePlayersStore{
		listed: []models.Server{{ID: 12, UUID: uuidMine}, {ID: 13, UUID: uuidQuiet}},
		routes: []store.RemoteRoute{
			{Domain: "a.example.com", ServerUUID: uuidMine},
			{Domain: "b.example.com", ServerUUID: uuidMine},
			{Domain: "q.example.com", ServerUUID: uuidQuiet},
			{Domain: "t.example.com", ServerUUID: uuidTheirs},
		},
	}
	for _, d := range []string{"a.example.com", "b.example.com", "q.example.com", "t.example.com", "orphan.example.com"} {
		fake.AddRoute("link-1", d, "mc-x", 25565)
	}
	fake.SetPlayers("a.example.com", 3, 1)
	fake.SetPlayers("b.example.com", 1, 0)
	fake.SetPlayers("t.example.com", 9, 9)
	fake.SetPlayers("orphan.example.com", 5, 5) // on the provider, no row here
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mode := func() (bool, bool) { return remote, known }
	rg := services.NewRemoteGateway(fs, func() *remotegw.Client { return api })
	return &ServerHandler{d: testDeps(Deps{Store: fs, Redis: rdb, GatewayRemote: mode, RemoteGateway: rg})}, fake, rdb
}

// Remote mode: the provider's counts per address, summed per server through
// the local rows, for the caller's servers only.
func TestRemotePlayersPerServer(t *testing.T) {
	h, fake, _ := newRemotePlayersHandler(t, true, true)
	code, body := playersBody(t, h)
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%d %s: %v", code, body, err)
	}
	at, _ := out["countsAt"].(string)
	delete(out, "countsAt")
	got, _ := json.Marshal(out)
	want := `{"editions":{"12":{"bedrock":1,"java":4}},"players":{"12":5},"stale":false}`
	if code != http.StatusOK || string(got) != want {
		t.Fatalf("%d %s, want %s", code, got, want)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("countsAt %q: %v", at, err)
	}
	if n := playersCalls(fake); n != 1 {
		t.Fatalf("%d provider reads", n)
	}
}

// A provider that cannot be read before any good answer: no counts, 200.
func TestRemotePlayersProviderDown(t *testing.T) {
	h, fake, _ := newRemotePlayersHandler(t, true, true)
	fake.FailNext(1, http.StatusBadGateway, "", 0)
	code, body := playersBody(t, h)
	if want := `{"editions":{},"players":{},"stale":false}` + "\n"; code != http.StatusOK || body != want {
		t.Fatalf("%d %s, want %s", code, body, want)
	}
}

// A provider older than GET /players answers its router's 404: no counts,
// no error.
func TestRemotePlayersOldProvider(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Not found"}`))
	}))
	t.Cleanup(old.Close)
	h, _, _ := newRemotePlayersHandler(t, true, true)
	h.d.RemoteGateway = services.NewRemoteGateway(h.d.Store, func() *remotegw.Client { return remotegw.New(old.URL, func() string { return "k" }) })
	code, body := playersBody(t, h)
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if code != http.StatusOK || len(out["players"].(map[string]any)) != 0 || len(out["editions"].(map[string]any)) != 0 || out["stale"] != false {
		t.Fatalf("%d %s, want no counts", code, body)
	}
}

// Review M-4: a route table that cannot be read gives no counts and one log
// line with context, at most once a minute however often the sidebar polls.
func TestRemotePlayersLogsAFailedRouteRead(t *testing.T) {
	h, _, _ := newRemotePlayersHandler(t, true, true)
	h.d.Store.(*remotePlayersStore).routesErr = errors.New("pq: connection reset by peer")
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	playersLogged.Store(0)
	for i := 0; i < 5; i++ {
		if code, body := playersBody(t, h); code != http.StatusOK || !strings.HasPrefix(body, `{"editions":{},"players":{}`) {
			t.Fatalf("%d %s", code, body)
		}
	}
	if n := strings.Count(logs.String(), "player counts: remote routes: pq: connection reset by peer"); n != 1 {
		t.Fatalf("%d log lines for 5 polls, want 1:\n%s", n, logs.String())
	}
}

// The sidebar polls; the platform's key is read at most once per 5 s window.
func TestRemotePlayersOneProviderReadPerWindow(t *testing.T) {
	h, fake, _ := newRemotePlayersHandler(t, true, true)
	for i := 0; i < 20; i++ {
		if code, body := playersBody(t, h); code != http.StatusOK || !strings.Contains(body, `"12":5`) {
			t.Fatalf("poll %d: %d %s", i, code, body)
		}
	}
	if n := playersCalls(fake); n != 1 {
		t.Fatalf("%d provider reads for 20 polls, want 1", n)
	}
}

// Local mode answers byte for byte as before (golden body captured from the
// handler before the remote branch existed) and never asks the provider.
func TestPlayersLocalModeUnchanged(t *testing.T) {
	h, fake, rdb := newRemotePlayersHandler(t, false, true)
	ctx := context.Background()
	rdb.Set(ctx, "edge:registry:e1", `{"edge_id":"e1"}`, 0)
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:edge:e1:stats", Values: map[string]any{
		"data": `{"players_by_server":{"` + uuidMine + `":3,"` + uuidTheirs + `":7}}`,
	}})
	code, body := playersBody(t, h)
	if want := `{"players":{"12":3}}` + "\n"; code != http.StatusOK || body != want {
		t.Fatalf("%d %q, want %q", code, body, want)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("local mode asked the provider: %v", fake.Calls())
	}
}

// An unread mode keeps the answer the local-only gate gave: 503.
func TestPlayersUnknownModeAnswers503(t *testing.T) {
	h, fake, _ := newRemotePlayersHandler(t, true, false)
	code, body := playersBody(t, h)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "gateway_provider_unknown") || len(fake.Calls()) != 0 {
		t.Fatalf("%d %s, calls %v", code, body, fake.Calls())
	}
}
