package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
)

func signedSession(t *testing.T, c *Claims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func sessionAt(user *models.User, id string, loginAt, issuedAt, exp time.Time) *Claims {
	return &Claims{
		Username: user.Username,
		PwdFp:    passwordFingerprint(sessionKey(user)),
		LoginAt:  loginAt.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        id,
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
}

// cookieGet runs one GET through AuthMiddleware with the session in the cookie
// and returns the response and, if one was set, the new session token.
func cookieGet(t *testing.T, h *AuthHandler, tok string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/profile", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.AuthMiddleware(func(http.ResponseWriter, *http.Request) {})(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return rec, c.Value
		}
	}
	return rec, ""
}

func parseSession(t *testing.T, tok string) *Claims {
	t.Helper()
	c := &Claims{}
	if _, err := jwt.ParseWithClaims(tok, c, func(*jwt.Token) (interface{}, error) { return []byte(testJWTSecret), nil }); err != nil {
		t.Fatal(err)
	}
	return c
}

// A session used to be a flat 24 hours with nothing to extend it, so everyone
// was signed out once a day. A session in use now slides to 30 days after its
// last use, keeps its id and login time, and stops at 90 days after login.
func TestASessionInUseSlides(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "$2a$10$hash"}
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: user}}, jwtKey: []byte(testJWTSecret)}
	now := time.Now()

	t.Run("used after an hour, it is extended", func(t *testing.T) {
		login := now.Add(-10 * 24 * time.Hour)
		rec, fresh := cookieGet(t, h, signedSession(t, sessionAt(user, "s1", login, now.Add(-2*time.Hour), now.Add(5*24*time.Hour))))
		if rec.Code != http.StatusOK || fresh == "" {
			t.Fatalf("status %d, new token %v: a session in use was not extended", rec.Code, fresh != "")
		}
		c := parseSession(t, fresh)
		if c.ID != "s1" || c.LoginAt != login.Unix() {
			t.Fatalf("id %q login %d: the slide must keep the session's id and login time", c.ID, c.LoginAt)
		}
		if d := c.ExpiresAt.Time.Sub(now.Add(sessionTTL)); d.Abs() > time.Minute {
			t.Fatalf("expires %v, want 30 days from now", c.ExpiresAt.Time)
		}
	})

	t.Run("used again within the hour, nothing is re-issued", func(t *testing.T) {
		_, fresh := cookieGet(t, h, signedSession(t, sessionAt(user, "s1", now.Add(-time.Hour), now.Add(-time.Minute), now.Add(29*24*time.Hour))))
		if fresh != "" {
			t.Fatal("a session issued a minute ago was re-issued")
		}
	})

	t.Run("it stops at 90 days after login", func(t *testing.T) {
		login := now.Add(-80 * 24 * time.Hour)
		_, fresh := cookieGet(t, h, signedSession(t, sessionAt(user, "s1", login, now.Add(-2*time.Hour), now.Add(24*time.Hour))))
		c := parseSession(t, fresh)
		if d := c.ExpiresAt.Time.Sub(login.Add(sessionMaxAge)); d.Abs() > time.Second {
			t.Fatalf("expires %v, want the cap %v", c.ExpiresAt.Time, login.Add(sessionMaxAge))
		}
	})

	t.Run("a 24-hour token from before this release is carried over", func(t *testing.T) {
		old := &Claims{Username: user.Username, PwdFp: passwordFingerprint(sessionKey(user)),
			RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(now.Add(20 * time.Hour))}}
		rec, fresh := cookieGet(t, h, signedSession(t, old))
		if rec.Code != http.StatusOK || fresh == "" {
			t.Fatalf("status %d: a session minted before the release was not extended", rec.Code)
		}
		if c := parseSession(t, fresh); c.ID == "" || c.LoginAt != now.Add(-4*time.Hour).Unix() {
			t.Fatalf("id %q login %v: want a new id and the login read off the old expiry", c.ID, time.Unix(c.LoginAt, 0))
		}
	})
}

// Signing out ends the session itself, so a copy of the cookie stops working
// too; a different session of the same account is untouched. Redis being down
// lets sessions through rather than signing the platform out.
func TestSigningOutEndsThatSession(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "$2a$10$hash"}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: user}, Redis: rdb}, jwtKey: []byte(testJWTSecret)}
	now := time.Now()
	login := now.Add(-time.Hour)
	mine := signedSession(t, sessionAt(user, "mine", login, now, now.Add(sessionTTL)))
	other := signedSession(t, sessionAt(user, "other", login, now, now.Add(sessionTTL)))

	// From another site the cookie may be cleared, but the session is not ended.
	cross := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	cross.Header.Set("Origin", "https://elsewhere.example")
	cross.AddCookie(&http.Cookie{Name: sessionCookieName, Value: mine})
	h.Logout(httptest.NewRecorder(), cross)
	if rec, _ := cookieGet(t, h, mine); rec.Code != http.StatusOK {
		t.Fatalf("a cross-site sign-out ended the session: %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("Origin", "http://example.com")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: mine})
	h.Logout(httptest.NewRecorder(), req)

	if rec, _ := cookieGet(t, h, mine); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the signed-out session still works: %d", rec.Code)
	}
	if ttl := mr.TTL(revokedSessionKey("mine")); ttl < sessionMaxAge-2*time.Hour {
		t.Fatalf("revocation lives %v, want until the 90-day cap a slid copy could reach", ttl)
	}
	if rec, _ := cookieGet(t, h, other); rec.Code != http.StatusOK {
		t.Fatalf("another session of the account was ended too: %d", rec.Code)
	}

	mr.Close()
	if rec, _ := cookieGet(t, h, other); rec.Code != http.StatusOK {
		t.Fatalf("with Redis down a valid session got %d", rec.Code)
	}
}

func bearerGet(t *testing.T, h *AuthHandler, tok string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/profile", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.AuthMiddleware(func(http.ResponseWriter, *http.Request) {})(rec, req)
	return rec
}

// Only a cookie session slides, and not past its cap. A bearer has no jar to
// take the new token, and a setup token is not a session at all.
func TestOnlyACookieSessionSlidesAndNotPastTheCap(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "$2a$10$hash"}
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: user}}, jwtKey: []byte(testJWTSecret)}
	now := time.Now()
	old := sessionAt(user, "s1", now.Add(-10*24*time.Hour), now.Add(-2*time.Hour), now.Add(5*24*time.Hour))

	rec := bearerGet(t, h, signedSession(t, old))
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("status %d, cookies %v: a bearer request was given a cookie", rec.Code, rec.Result().Cookies())
	}

	login := now.Add(-sessionMaxAge + time.Hour)
	atCap := sessionAt(user, "s1", login, now.Add(-2*time.Hour), login.Add(sessionMaxAge))
	if _, fresh := cookieGet(t, h, signedSession(t, atCap)); fresh != "" {
		t.Fatal("a session at its 90-day cap was re-issued")
	}
}

// The token Beam's native side gets is a copy of the caller's session: same id
// and login time, so signing out ends it and it cannot outrun the cap.
func TestBeamsBearerIsTheSameSession(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "$2a$10$hash"}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: user}, Redis: rdb}, jwtKey: []byte(testJWTSecret)}
	now := time.Now()
	login := now.Add(-80 * 24 * time.Hour)
	mine := signedSession(t, sessionAt(user, "s9", login, now.Add(-time.Minute), now.Add(10*24*time.Hour)))

	req := httptest.NewRequest(http.MethodPost, "/api/auth/session-token", nil)
	req.Host = wailsHost
	req.Header.Set("Origin", "wails://wails")
	req.Header.Set("Authorization", "Bearer "+mine)
	rec := httptest.NewRecorder()
	h.AuthMiddleware(h.SessionToken)(rec, req)
	var body struct{ Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Token == "" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	c := parseSession(t, body.Token)
	if c.ID != "s9" || c.LoginAt != login.Unix() || c.ExpiresAt.Time.After(login.Add(sessionMaxAge).Add(time.Second)) {
		t.Fatalf("copy id %q login %v exp %v: want the caller's session, capped", c.ID, time.Unix(c.LoginAt, 0), c.ExpiresAt.Time)
	}

	out := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	out.Header.Set("Origin", "http://example.com")
	out.AddCookie(&http.Cookie{Name: sessionCookieName, Value: mine})
	h.Logout(httptest.NewRecorder(), out)
	if rec := bearerGet(t, h, body.Token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("Beam's copy outlived the sign-out: %d", rec.Code)
	}
}

// A token from before sessions had ids gets one from the token itself, so it
// keeps it across slides and a sign-out still reaches every copy.
func TestALegacySessionCanBeSignedOut(t *testing.T) {
	user := &models.User{ID: "u1", Username: "alice", Password: "$2a$10$hash"}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: user}, Redis: rdb}, jwtKey: []byte(testJWTSecret)}
	legacy := signedSession(t, &Claims{Username: user.Username, PwdFp: passwordFingerprint(sessionKey(user)),
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(20 * time.Hour))}})

	_, slid := cookieGet(t, h, legacy)
	if slid == "" {
		t.Fatal("the legacy session was not carried over")
	}
	out := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	out.Header.Set("Origin", "http://example.com")
	out.AddCookie(&http.Cookie{Name: sessionCookieName, Value: legacy})
	h.Logout(httptest.NewRecorder(), out)
	if rec, _ := cookieGet(t, h, slid); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the slid copy outlived signing out the original: %d", rec.Code)
	}
}
