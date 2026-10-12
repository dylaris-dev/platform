package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	beamauth "dylaris-pkg/beam/auth"
)

const testJWTSecret = "test-jwt-secret-value-not-a-real-secret"

// middlewareUnder returns AuthMiddleware wrapped around a handler that records
// whether it ran. state is non-nil but empty: every rejection under test happens
// before the store lookup.
func middlewareUnder(t *testing.T) (http.Handler, *bool) {
	t.Helper()
	h := &AuthHandler{state: &AppState{}, jwtKey: []byte(testJWTSecret)}
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	return h.AuthMiddleware(next), &reached
}

// BEAM_JWT_SECRET is wired to JWT_SECRET in both compose files, on purpose - so
// a beam ticket is HS256-signed with the panel's session key. It carries
// "username" under the same JSON name a session does and no purpose at all, so
// the purpose denylist never saw it and it authenticated as its user for the
// ticket's full 30 minutes.
//
// It is not the user's own ticket that matters: the ticket travels to the beam
// app, through the gateway relay, and to the node - which for BYON is the
// customer's own machine.
func TestABeamTicketIsNotAPanelSession(t *testing.T) {
	ticket, err := beamauth.SignBeamTicket(testJWTSecret, beamauth.BeamClaims{
		ServerUUID: "srv-1",
		NodeID:     "node-1",
		Username:   "victim-admin",
		IsAdmin:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	mw, reached := middlewareUnder(t)
	req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.Header.Set("Authorization", "Bearer "+ticket)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if *reached {
		t.Fatal("a beam ticket reached the handler as an authenticated session")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// The mirror rule: a session token is not a beam ticket either. It has no
// issuer, and the node/relay must not treat "signed with this key" as proof of
// what the token is for.
func TestAPanelSessionIsNotABeamTicket(t *testing.T) {
	h := &AuthHandler{state: &AppState{}, jwtKey: []byte(testJWTSecret)}
	session, err := h.IssueToken("someone", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beamauth.ValidateBeamTicket(testJWTSecret, session); err == nil {
		t.Fatal("a panel session token was accepted as a beam ticket")
	}
}

// The guard must not swallow the tokens it exists to let through.
func TestAnOrdinarySessionStillAuthenticates(t *testing.T) {
	h := &AuthHandler{state: &AppState{}, jwtKey: []byte(testJWTSecret)}
	session, err := h.IssueToken("someone", false, "")
	if err != nil {
		t.Fatal(err)
	}

	mw, reached := middlewareUnder(t)
	req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !*reached {
		t.Fatalf("an ordinary session token was rejected with %d", rec.Code)
	}
}

// The two named purposes have to keep behaving as before, or this pass traded
// one hole for another.
func TestTabProxyTicketStillRejectedOnTheAPI(t *testing.T) {
	h := &AuthHandler{state: &AppState{}, jwtKey: []byte(testJWTSecret)}
	ticket, err := h.IssueTabProxyTicket("", "someone", false, 1, 2, false)
	if err != nil {
		t.Fatal(err)
	}

	mw, reached := middlewareUnder(t)
	req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.Header.Set("Authorization", "Bearer "+ticket)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if *reached {
		t.Fatal("a tab-proxy ticket reached the API handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}
