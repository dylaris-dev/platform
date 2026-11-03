package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
)

func ticketFixture(t *testing.T, user *models.User) (*AuthHandler, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &AuthHandler{
		d:      Deps{StoreEnabled: true, Store: &authResolveFakeStore{user: user}, Redis: rdb},
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

// A plain sign-out ends the session; a ticket it minted ends with it. A stream
// reconnecting every few minutes used to keep that ticket sliding for good. A
// ticket minted before tickets named their session still works.
func TestSSETicketEndsWithItsSession(t *testing.T) {
	h, mr := ticketFixture(t, &models.User{ID: "u1", Username: "alice", Password: "hash-1"})
	req := httptest.NewRequest(http.MethodPost, "/api/sse-ticket", nil)
	ctx := context.WithValue(req.Context(), "username", "alice")
	ctx = reqctx.WithSessionClaims(ctx, &Claims{RegisteredClaims: jwt.RegisteredClaims{ID: "s1"}})
	rec := httptest.NewRecorder()
	h.MintSSETicket(rec, req.WithContext(ctx))
	var out struct{ Ticket string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	if _, called := ticketGet(h, "/api/system/events?ticket="+out.Ticket); !called {
		t.Fatal("a live session's ticket was refused")
	}
	mr.Set(revokedSessionKey("s1"), "1")
	if code, called := ticketGet(h, "/api/system/events?ticket="+out.Ticket); called || code != http.StatusUnauthorized {
		t.Fatalf("after sign-out: status %d, next called %v; want 401", code, called)
	}

	mr.Set("sse:ticket:pre", "alice"+sseTicketSep+passwordFingerprint(sessionKey(&models.User{Password: "hash-1"})))
	if _, called := ticketGet(h, "/api/system/events?ticket=pre"); !called {
		t.Fatal("a ticket minted before tickets named their session was refused")
	}
}

// A tab-proxy ticket names the session that minted it, and stops with it.
func TestATabProxyTicketEndsWithItsSession(t *testing.T) {
	h, mr := ticketFixture(t, &models.User{ID: "u1", Username: "alice", Password: "hash-1"})
	tk, err := h.IssueTabProxyTicket("s2", "alice", false, 1, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ParseTabProxyTicket(tk); err != nil {
		t.Fatalf("a live session's ticket was refused: %v", err)
	}
	mr.Set(revokedSessionKey("s2"), "1")
	if _, err := h.ParseTabProxyTicket(tk); err == nil {
		t.Fatal("the ticket outlived its session's sign-out")
	}
}

// The ticket slides forward when a stream opens, not while it runs. A stream
// that lived as long as the ticket reconnected with one that had just expired,
// and the console went dead until a reload.
func TestStreamsEndBeforeTheirTicket(t *testing.T) {
	if httpx.SSEMaxStreamAge() >= sseTicketTTL {
		t.Fatalf("sseMaxStreamAge %v must be shorter than sseTicketTTL %v, or the reconnect carries an expired ticket", httpx.SSEMaxStreamAge(), sseTicketTTL)
	}
	// Room for the reconnect itself: the browser waits the retry interval,
	// then the request has to arrive.
	if sseTicketTTL-httpx.SSEMaxStreamAge() < 30*time.Second {
		t.Fatalf("only %v between stream end and ticket expiry", sseTicketTTL-httpx.SSEMaxStreamAge())
	}
	if httpx.SSERetryLine != "retry: 1000\n\n" {
		t.Fatalf("httpx.SSERetryLine = %q", httpx.SSERetryLine)
	}
}

// Beam reads the streams cross-origin, because its asset server cannot pass a
// stream through. The allowance is for the webview origin on a valid ticket
// only: any other origin, or no ticket, gets no CORS header to read with.
func TestSSETicketAllowsOnlyTheBeamOrigin(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "hash-1"}
	h, _ := ticketFixture(t, user)
	tk := mintTicket(t, h, "alice")

	get := func(path, origin string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.AuthMiddleware(func(http.ResponseWriter, *http.Request) {})(rec, req)
		return rec.Header().Get("Access-Control-Allow-Origin")
	}
	for _, origin := range []string{"http://wails.localhost", "wails://wails"} {
		if got := get("/api/system/events?ticket="+tk, origin); got != origin {
			t.Errorf("%s: Allow-Origin = %q, want the webview origin", origin, got)
		}
	}
	if got := get("/api/system/events?ticket="+tk, "https://evil.example"); got != "" {
		t.Errorf("a foreign origin was allowed: %q", got)
	}
	if got := get("/api/system/events?ticket=bogus", "http://wails.localhost"); got != "" {
		t.Errorf("an invalid ticket still got a CORS allowance: %q", got)
	}
}
