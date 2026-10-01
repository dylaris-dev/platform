package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const identitySourceSecret = "test-jwt-secret-value-not-a-real-secret"

// TestAuthMiddleware_AdminFlagComesFromTheRowNotTheClaim pins that a session's
// privilege is read from users.is_admin on every request, not from the isAdmin
// claim signed into the token at login.
//
// Sessions last 24 hours. PUT /admin/users/{id}/role writes users.is_admin at
// once, but the demoted account kept presenting a token that still said
// isAdmin:true - and that is what reached the request context, which
// authz.Resolve short-circuits on to grant EVERY capability. So a revoked admin
// stayed a full admin for up to a day, holding users.write and panelroles.write,
// which is long enough to simply put the role back.
//
// The row is loaded by the middleware anyway, one line earlier, to decide
// whether the account still exists.
func TestAuthMiddleware_AdminFlagComesFromTheRowNotTheClaim(t *testing.T) {
	tests := []struct {
		name      string
		claim     bool // what the token was signed with
		row       bool // what the database says now
		wantAdmin bool
	}{
		{
			name:      "demoted since login: the claim must not survive the role change",
			claim:     true,
			row:       false,
			wantAdmin: false,
		},
		{
			name:      "promoted since login: takes effect without waiting for a new token",
			claim:     false,
			row:       true,
			wantAdmin: true,
		},
		{name: "unchanged admin", claim: true, row: true, wantAdmin: true},
		{name: "unchanged user", claim: false, row: false, wantAdmin: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &authResolveFakeStore{
				user: &models.User{ID: "user-1", Username: "alice", IsAdmin: tt.row},
			}
			h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: fake}, jwtKey: []byte(identitySourceSecret)}

			token, err := h.IssueToken("alice", tt.claim, "")
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}

			var gotAdmin any
			next := func(w http.ResponseWriter, r *http.Request) {
				gotAdmin = r.Context().Value("isAdmin")
			}

			req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			h.AuthMiddleware(next)(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if gotAdmin != tt.wantAdmin {
				t.Errorf("isAdmin in context = %#v, want %v (token claim was %v, row says %v)",
					gotAdmin, tt.wantAdmin, tt.claim, tt.row)
			}
		})
	}
}

// TestAuthMiddleware_SSETicketFailsClosed is the ticket branch's half of
// TestAuthMiddleware_UserResolutionFailsClosed. That fix landed on the Bearer
// path only; this branch kept swallowing the lookup error and continuing with
// isAdmin=false and no userID at all.
//
// GET-only looks like it bounds the damage and does not: ListLinkRoutes is a GET
// that reads userID with a bare .(string), so an absent value panics the request
// instead of filtering it to nothing. And the reachable case needs no outage -
// the ticket outlives the account and its TTL is refreshed on every accepted
// request, so a deleted user's EventSource keeps its own ticket alive.
func TestAuthMiddleware_SSETicketFailsClosed(t *testing.T) {
	tests := []struct {
		name           string
		fake           *authResolveFakeStore
		wantStatus     int
		wantNext       bool
		wantTicketGone bool
	}{
		{
			name:           "account deleted while the ticket was still sliding",
			fake:           &authResolveFakeStore{err: sql.ErrNoRows},
			wantStatus:     http.StatusUnauthorized,
			wantTicketGone: true,
		},
		{
			name:           "renamed user: the ticket stores the old name",
			fake:           &authResolveFakeStore{},
			wantStatus:     http.StatusUnauthorized,
			wantTicketGone: true,
		},
		{
			// A blip must not read as bad credentials, and must not hand the
			// handler a session with no owner either.
			name:       "database fault",
			fake:       &authResolveFakeStore{err: errors.New("connection reset by peer")},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "resolved - the stream proceeds with a real identity",
			fake:       &authResolveFakeStore{user: &models.User{ID: "user-1", Username: "alice", IsAdmin: true}},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, err := miniredis.Run()
			if err != nil {
				t.Fatalf("miniredis: %v", err)
			}
			t.Cleanup(mr.Close)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

			const ticket = "ticket-value"
			if err := rdb.Set(context.Background(), "sse:ticket:"+ticket, "alice", sseTicketTTL).Err(); err != nil {
				t.Fatalf("seed ticket: %v", err)
			}

			h := &AuthHandler{
				state:  &AppState{StoreEnabled: true, Store: tt.fake, Redis: rdb},
				jwtKey: []byte(identitySourceSecret),
			}

			var gotUserID, gotAdmin any
			called := false
			next := func(w http.ResponseWriter, r *http.Request) {
				called = true
				gotUserID = r.Context().Value("userID")
				gotAdmin = r.Context().Value("isAdmin")
			}

			req := httptest.NewRequest(http.MethodGet, "/api/system/events?ticket="+ticket, nil)
			rec := httptest.NewRecorder()
			h.AuthMiddleware(next)(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if called != tt.wantNext {
				t.Fatalf("next called = %v, want %v", called, tt.wantNext)
			}
			if tt.wantTicketGone && mr.Exists("sse:ticket:"+ticket) {
				t.Error("the ticket survived a rejected request; its TTL is refreshed on every use, so it would slide forever")
			}
			if !tt.wantNext {
				return
			}
			if id, ok := gotUserID.(string); !ok || id == "" {
				t.Errorf("userID in context = %#v, want a non-empty string", gotUserID)
			}
			if gotAdmin != true {
				t.Errorf("isAdmin in context = %#v, want true (the row says so)", gotAdmin)
			}
		})
	}
}
