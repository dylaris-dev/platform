package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
)

func ticketFixture(t *testing.T, user *models.User) (*AuthHandler, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &AuthHandler{
		state:  &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: user}, Redis: rdb},
		jwtKey: []byte(identitySourceSecret),
	}, mr
}

func mintTicket(t *testing.T, h *AuthHandler, username string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sse-ticket", nil)
	req = req.WithContext(context.WithValue(req.Context(), "username", username))
	rec := httptest.NewRecorder()
	h.MintSSETicket(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ Ticket string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Ticket
}

func ticketGet(h *AuthHandler, path string) (int, bool) {
	called := false
	rec := httptest.NewRecorder()
	h.AuthMiddleware(func(http.ResponseWriter, *http.Request) { called = true })(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, called
}

// A ticket used to open every GET behind AuthMiddleware - file reads, server
// lists, admin reads - and slid forward on each use. It now opens the streams
// an EventSource reads and nothing else.
func TestSSETicketOpensOnlyTheStreams(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "hash-1"}
	h, _ := ticketFixture(t, user)
	tk := mintTicket(t, h, "alice")

	for _, p := range []string{"/api/system/events", "/api/servers/7/console/stream", "/api/servers/7/stats/stream"} {
		if code, called := ticketGet(h, p+"?ticket="+tk); !called {
			t.Errorf("%s: the ticket was refused (%d)", p, code)
		}
	}
	for _, p := range []string{"/api/servers", "/api/servers/7/files/read", "/api/admin/users", "/api/servers/7/console/history"} {
		if _, called := ticketGet(h, p+"?ticket="+tk); called {
			t.Errorf("%s: a ticket opened a route that is not a stream", p)
		}
	}
}

// A password change ends a session token; it now ends a ticket too, which used
// to keep reading for as long as its holder kept it sliding.
func TestSSETicketEndsWithAPasswordChange(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "hash-1"}
	h, mr := ticketFixture(t, user)
	tk := mintTicket(t, h, "alice")

	user.Password = "hash-2"
	if code, called := ticketGet(h, "/api/system/events?ticket="+tk); called || code != http.StatusUnauthorized {
		t.Fatalf("after a password change: status %d, next called %v; want 401", code, called)
	}
	if mr.Exists("sse:ticket:" + tk) {
		t.Error("the ticket was kept after the password changed")
	}
}

// A ticket minted before this release carries no fingerprint. It is honoured so
// a stream open across the deploy reconnects, but no longer extended.
func TestALegacySSETicketIsNotExtended(t *testing.T) {
	h, mr := ticketFixture(t, &models.User{ID: "u1", Username: "alice", Password: "hash-1"})
	mr.Set("sse:ticket:old", "alice")
	mr.SetTTL("sse:ticket:old", sseTicketTTL/5)
	if _, called := ticketGet(h, "/api/system/events?ticket=old"); !called {
		t.Fatal("a ticket from before the deploy was refused outright")
	}
	if ttl := mr.TTL("sse:ticket:old"); ttl > sseTicketTTL/5 {
		t.Fatalf("a legacy ticket was extended to %s", ttl)
	}
}
