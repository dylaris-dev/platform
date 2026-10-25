package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"dylaris-core/models"
)

// Anyone holding the signing key - until 2026.10.25.8 that included the beam
// relay and cluster nodes, whose BEAM_JWT_SECRET was the same value - could
// sign {"username": "<an admin>"} and be that admin: authorization comes from
// the user row, and the only check binding a token to the account was skipped
// when the token carried no password fingerprint. Every token Core mints for
// the panel carries one, and an expiry; a token without them is hand-made.
func TestAHandSignedSessionIsRefused(t *testing.T) {
	const hash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	admin := &models.User{ID: "admin-1", Username: "root", Password: hash, IsAdmin: true}
	h := &AuthHandler{
		state:  &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: admin}},
		jwtKey: []byte(testJWTSecret),
	}
	sign := func(c *Claims) string {
		t.Helper()
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(h.jwtKey)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	later := jwt.NewNumericDate(time.Now().Add(time.Hour))
	real, err := h.IssueToken("root", true, sessionKey(admin))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"a real session", real, http.StatusOK},
		{"no fingerprint", sign(&Claims{Username: "root", IsAdmin: true, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: later}}), http.StatusUnauthorized},
		{"no expiry", sign(&Claims{Username: "root", PwdFp: passwordFingerprint(sessionKey(admin))}), http.StatusUnauthorized},
		{"a guessed fingerprint", sign(&Claims{Username: "root", PwdFp: "0000000000000000", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: later}}), http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			h.AuthMiddleware(func(http.ResponseWriter, *http.Request) { reached = true })(rec, req)
			if rec.Code != tc.want || reached != (tc.want == http.StatusOK) {
				t.Fatalf("status %d reached %v, want %d", rec.Code, reached, tc.want)
			}
		})
	}
}

// An account with no password at all has nothing to bind to: empty on both
// sides still matches, so such a session keeps working.
func TestAPasswordlessAccountSessionStillWorks(t *testing.T) {
	u := &models.User{ID: "u-1", Username: "nopw"}
	h := &AuthHandler{
		state:  &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: u}},
		jwtKey: []byte(testJWTSecret),
	}
	tok, err := h.IssueToken("nopw", false, sessionKey(u))
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.AuthMiddleware(func(http.ResponseWriter, *http.Request) { reached = true })(rec, req)
	if !reached {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
