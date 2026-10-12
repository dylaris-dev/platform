package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// newTestAuthHandler builds an AuthHandler with only the jwtKey field set.
// IssueToken and IsAdminToken never touch h.state, so state stays nil.
func newTestAuthHandler() *AuthHandler {
	return &AuthHandler{jwtKey: []byte("test-jwt-secret-value-not-a-real-secret")}
}

func TestIssueToken_HS256(t *testing.T) {
	h := newTestAuthHandler()
	tokenString, err := h.IssueToken("alice", true, "")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (interface{}, error) {
		return h.jwtKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !tok.Valid {
		t.Fatalf("token did not verify: err=%v valid=%v", err, tok.Valid)
	}
	if tok.Method.Alg() != "HS256" {
		t.Errorf("alg = %q, want HS256", tok.Method.Alg())
	}
	if claims.Username != "alice" {
		t.Errorf("Username = %q, want alice", claims.Username)
	}
	if !claims.IsAdmin {
		t.Error("IsAdmin = false, want true")
	}
	if claims.ExpiresAt == nil || claims.ExpiresAt.Before(time.Now().Add(sessionTTL-time.Minute)) {
		t.Errorf("ExpiresAt = %v, want sessionTTL from now", claims.ExpiresAt)
	}
}

func TestIssueToken_NonAdmin(t *testing.T) {
	h := newTestAuthHandler()
	tokenString, err := h.IssueToken("bob", false, "")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	claims := &Claims{}
	if _, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (interface{}, error) {
		return h.jwtKey, nil
	}, jwt.WithValidMethods([]string{"HS256"})); err != nil {
		t.Fatalf("token did not verify: %v", err)
	}
	if claims.IsAdmin {
		t.Error("IsAdmin = true, want false")
	}
}

func TestIsAdminToken(t *testing.T) {
	h := newTestAuthHandler()
	adminTok, err := h.IssueToken("admin-user", true, "")
	if err != nil {
		t.Fatalf("IssueToken(admin): %v", err)
	}
	userTok, err := h.IssueToken("regular-user", false, "")
	if err != nil {
		t.Fatalf("IssueToken(user): %v", err)
	}

	cases := []struct {
		name    string
		makeReq func() *http.Request
		want    bool
	}{
		{"valid admin bearer token", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.Header.Set("Authorization", "Bearer "+adminTok)
			return r
		}, true},
		{"valid non-admin bearer token", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.Header.Set("Authorization", "Bearer "+userTok)
			return r
		}, false},
		{"admin token via query param", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/x?token="+adminTok, nil)
		}, true},
		{"no token at all", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/x", nil)
		}, false},
		{"garbage bearer token", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.Header.Set("Authorization", "Bearer not-a-jwt")
			return r
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.IsAdminToken(tc.makeReq()); got != tc.want {
				t.Errorf("IsAdminToken = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsAdminToken_RejectsNonHS256Alg proves the alg allowlist
// (jwt.WithValidMethods(["HS256"])) is doing real work: a token forged with
// a DIFFERENT signing algorithm but the SAME key bytes must be rejected,
// even though the signature is technically valid under that other
// algorithm. This pairs with the identical guard in AuthMiddleware.
func TestIsAdminToken_RejectsNonHS256Alg(t *testing.T) {
	h := newTestAuthHandler()
	claims := &Claims{
		Username:         "attacker",
		IsAdmin:          true,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodHS384, claims)
	forgedString, err := forged.SignedString(h.jwtKey)
	if err != nil {
		t.Fatalf("sign forged token: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer "+forgedString)
	if h.IsAdminToken(r) {
		t.Fatal("IsAdminToken must reject a non-HS256 token even with a valid signature under the same key")
	}
}

// --- Tab-proxy ticket (WS5 Task 8 fast-follow: cookie-only proxy auth) ---

// TestIssueTabProxyTicket_ClaimsAndExpiry checks the minted ticket's shape:
// right purpose, right server/tab/identity/readOnly claims, and an expiry
// ~tabProxyTicketTTL out - independent of the mint HTTP handler.
func TestIssueTabProxyTicket_ClaimsAndExpiry(t *testing.T) {
	h := newTestAuthHandler()

	before := time.Now()
	tok, err := h.IssueTabProxyTicket("", "alice", true, 42, 7, true)
	after := time.Now()
	if err != nil {
		t.Fatalf("IssueTabProxyTicket: %v", err)
	}

	claims, err := h.ParseTabProxyTicket(tok)
	if err != nil {
		t.Fatalf("ParseTabProxyTicket: %v", err)
	}
	if claims.Purpose != "tab_proxy" {
		t.Errorf("Purpose = %q, want tab_proxy", claims.Purpose)
	}
	if claims.Username != "alice" || !claims.IsAdmin {
		t.Errorf("identity = %q/%v, want alice/true", claims.Username, claims.IsAdmin)
	}
	if claims.ServerID != 42 || claims.TabID != 7 {
		t.Errorf("server/tab = %d/%d, want 42/7", claims.ServerID, claims.TabID)
	}
	if !claims.ReadOnly {
		t.Error("ReadOnly = false, want true")
	}
	if claims.ExpiresAt == nil {
		t.Fatal("ExpiresAt not set")
	}
	exp := claims.ExpiresAt.Time
	if exp.Before(before.Add(tabProxyTicketTTL-2*time.Second)) || exp.After(after.Add(tabProxyTicketTTL+2*time.Second)) {
		t.Errorf("ExpiresAt = %v, want ~%v from mint time", exp, tabProxyTicketTTL)
	}
}

// TestParseTabProxyTicket_RejectsNonTabProxyPurpose is the pure-claims
// counterpart to the proxy's cookie check: any Purpose other than
// "tab_proxy" (a normal session token, a "2fa_setup" token, or anything
// else) must fail ParseTabProxyTicket even with a valid signature.
func TestParseTabProxyTicket_RejectsNonTabProxyPurpose(t *testing.T) {
	h := newTestAuthHandler()

	cases := []struct {
		name    string
		purpose string
	}{
		{"empty (normal session)", ""},
		{"2fa_setup", "2fa_setup"},
		{"unrelated purpose", "something_else"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := &Claims{
				Username:         "owner",
				Purpose:          c.purpose,
				RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			signed, err := tok.SignedString(h.jwtKey)
			if err != nil {
				t.Fatalf("signing: %v", err)
			}
			if _, err := h.ParseTabProxyTicket(signed); err == nil {
				t.Errorf("ParseTabProxyTicket accepted Purpose=%q, want rejected", c.purpose)
			}
		})
	}
}

// TestAuthMiddleware_RejectsTabProxyPurposeToken is defense-in-depth beyond
// what the task strictly requires: a Purpose == "tab_proxy" ticket (minted by
// ProxyHandler.MintProxyAuth, meant to be validated only by the proxy's own
// cookie parsing in tab_proxy.go) must never authenticate a normal /api call
// through AuthMiddleware, even though it is signed with the same key as a
// real session token - mirroring the existing "2fa_setup" purpose gate.
func TestAuthMiddleware_RejectsTabProxyPurposeToken(t *testing.T) {
	h := &AuthHandler{state: &AppState{}, jwtKey: []byte("test-jwt-secret-value-not-a-real-secret")}

	ticket, err := h.IssueTabProxyTicket("", "owner", false, 1, 2, false)
	if err != nil {
		t.Fatalf("IssueTabProxyTicket: %v", err)
	}

	called := false
	next := func(w http.ResponseWriter, r *http.Request) { called = true }
	wrapped := h.AuthMiddleware(next)

	req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.Header.Set("Authorization", "Bearer "+ticket)
	rec := httptest.NewRecorder()

	wrapped(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (tab_proxy purpose token used as a normal Bearer)", rec.Code, http.StatusForbidden)
	}
	if called {
		t.Error("next handler was called - tab_proxy purpose token must be rejected before reaching it")
	}
}
