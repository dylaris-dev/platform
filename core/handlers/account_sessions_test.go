package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"dylaris-core/models"
)

func setupToken(t *testing.T) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
		Username: "alice", Purpose: "2fa_setup",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute))},
	}).SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A setup token reached PUT /api/auth/profile because the allowlist checked the
// path alone - and a password change there (to the same password) hands back a
// full session. "2FA required" then withheld nothing from anyone holding the
// password of an account that had not enrolled.
func TestASetupTokenOnlyReadsTheProfile(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   bool
	}{
		{http.MethodGet, true},
		{http.MethodPut, false},
	} {
		mw, reached := middlewareUnder(t)
		req := httptest.NewRequest(tc.method, "/api/auth/profile", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+setupToken(t))
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if *reached != tc.want {
			t.Fatalf("%s /api/auth/profile with a setup token: reached = %v, want %v (status %d)", tc.method, *reached, tc.want, rec.Code)
		}
	}
}

// Every session is bound to the account's session epoch. Epoch 0 is the bare
// password hash, so sessions issued before the epoch existed stay valid; a bump
// ends all of them.
func TestASessionEndsWhenTheEpochMoves(t *testing.T) {
	user := &models.User{ID: "user-1", Username: "alice", Password: "$2a$10$hash"}
	if passwordFingerprint(sessionKey(user)) != passwordFingerprint(user.Password) {
		t.Fatal("epoch 0 changed the fingerprint: every live session would end on deploy")
	}
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: &authResolveFakeStore{user: user}}, jwtKey: []byte(testJWTSecret)}
	token, err := h.IssueToken(user.Username, false, sessionKey(user))
	if err != nil {
		t.Fatal(err)
	}
	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.AuthMiddleware(func(http.ResponseWriter, *http.Request) {})(rec, req)
		return rec.Code
	}
	if code := call(); code != http.StatusOK {
		t.Fatalf("a current session was refused: %d", code)
	}
	user.SessionEpoch = 1
	if code := call(); code != http.StatusUnauthorized {
		t.Fatalf("a session from before the bump still works: %d", code)
	}
}

// sessionsFakeStore serves one user and counts epoch bumps.
type sessionsFakeStore struct {
	totpFakeStore
	user *models.User
}

func (f *sessionsFakeStore) GetUserByUsername(string) (*models.User, error) { return f.user, nil }
func (f *sessionsFakeStore) GetUserByID(string) (*models.User, error)       { return f.user, nil }
func (f *sessionsFakeStore) GetSetting(string) (string, error)              { return "", nil }
func (f *sessionsFakeStore) DisableUserTOTP(string) error                   { return nil }

// "Sign out everywhere" ends every session of the account and hands the
// caller a new one - logging out used to clear one cookie and nothing else.
func TestLogoutEverywhereEndsEverySessionButHandsBackOne(t *testing.T) {
	fs := &sessionsFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: "h"}}
	h := &AuthHandler{state: &AppState{Store: fs}, jwtKey: []byte(testJWTSecret)}
	rr := postJSONAs(t, h.LogoutEverywhere, "alice", struct{}{})
	if rr.Code != http.StatusOK || fs.epochBumps != 1 {
		t.Fatalf("status %d, bumps %d: %s", rr.Code, fs.epochBumps, rr.Body)
	}
	var fresh string
	for _, c := range rr.Result().Cookies() {
		if strings.Contains(c.Name, "dylaris_session") {
			fresh = c.Value
		}
	}
	if fresh == "" {
		t.Fatal("the caller was not handed a new session")
	}
	claims := &Claims{}
	if _, err := jwt.ParseWithClaims(fresh, claims, func(*jwt.Token) (interface{}, error) { return []byte(testJWTSecret), nil }); err != nil {
		t.Fatal(err)
	}
	if claims.PwdFp != passwordFingerprint(sessionKey(fs.user)) || fs.user.SessionEpoch != 1 {
		t.Fatal("the new session is not bound to the new epoch")
	}
}

// Enrolling while 2FA is on replaced the authenticator and the backup codes on
// the password alone; disabling, the way to switch, asks for a current code.
func TestEnrolmentIsRefusedWhileTwoFactorIsOn(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	secret := freshTOTPSecret(t)
	code, _ := totp.GenerateCode(secret, time.Now())
	fs := &sessionsFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: string(hash), Is2FAEnabled: true}}
	h := &AuthHandler{state: &AppState{Store: fs}, jwtKey: []byte(testJWTSecret)}
	rr := postJSONAs(t, h.VerifyTOTPHandler, "alice", VerifyTOTPRequest{Secret: secret, Code: code, Password: "pw"})
	if rr.Code != http.StatusConflict || fs.setCalls != 0 {
		t.Fatalf("status %d, writes %d: re-enrolment was not refused", rr.Code, fs.setCalls)
	}
}

// Turning 2FA on or off, and an operator's 2FA reset, end every session: one
// taken from the account must not outlive the owner securing it.
func TestASecondFactorChangeEndsTheSessions(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	secret := freshTOTPSecret(t)

	t.Run("enable", func(t *testing.T) {
		code, _ := totp.GenerateCode(secret, time.Now())
		fs := &sessionsFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: string(hash)}}
		h := &AuthHandler{state: &AppState{Store: fs}, jwtKey: []byte(testJWTSecret)}
		rr := postJSONAs(t, h.VerifyTOTPHandler, "alice", VerifyTOTPRequest{Secret: secret, Code: code, Password: "pw"})
		if rr.Code != http.StatusOK || fs.epochBumps != 1 {
			t.Fatalf("status %d, bumps %d", rr.Code, fs.epochBumps)
		}
	})
	t.Run("disable", func(t *testing.T) {
		code, _ := totp.GenerateCode(secret, time.Now())
		fs := &sessionsFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: string(hash), Is2FAEnabled: true, TOTPSecret: secret}}
		h := &AuthHandler{state: &AppState{Store: fs}, jwtKey: []byte(testJWTSecret)}
		rr := postJSONAs(t, h.DisableTOTPHandler, "alice", DisableTOTPRequest{Password: "pw", Code: code})
		if rr.Code != http.StatusOK || fs.epochBumps != 1 {
			t.Fatalf("status %d, bumps %d: %s", rr.Code, fs.epochBumps, rr.Body)
		}
	})
}

// The second factor at login is limited per account: the per-IP limiter alone
// let anyone holding the password guess codes from as many addresses as they
// had. Past the limit even the right code waits out the window.
func TestWrongCodesAreLimitedPerAccount(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	secret := freshTOTPSecret(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	fs := &loginFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: string(hash), Is2FAEnabled: true, TOTPSecret: secret}}
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: fs, Redis: rdb}, jwtKey: []byte(testJWTSecret)}

	login := func(code string) int {
		body, _ := json.Marshal(map[string]string{"username": "alice", "password": "pw", "totpCode": code})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		h.LoginHandler(rec, req)
		return rec.Code
	}
	for i := 0; i < wrongCodeLimit; i++ {
		if code := login("000000"); code != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: status %d, want 401", i+1, code)
		}
	}
	right, _ := totp.GenerateCode(secret, time.Now())
	if code := login(right); code != http.StatusTooManyRequests {
		t.Fatalf("past the limit the right code got %d, want 429", code)
	}
	if ttl := mr.TTL(wrongCodeKey("u1")); ttl <= 0 || ttl > wrongCodeWindow {
		t.Fatalf("the window has ttl %v", ttl)
	}
}

// successLoginStore lets a login complete.
type successLoginStore struct{ loginFakeStore }

func (f *successLoginStore) UpdateLastLoginAt(string) error { return nil }

// The attempt is counted BEFORE the code is checked. Counting after let a
// burst of parallel logins all read the old count and all guess; and a right
// code clears the count, so only misses add up.
func TestTheCodeLimitHoldsAgainstABurstAndClearsOnSuccess(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	secret := freshTOTPSecret(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	fs := &successLoginStore{loginFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: string(hash), Is2FAEnabled: true, TOTPSecret: secret}}}
	h := &AuthHandler{state: &AppState{StoreEnabled: true, Store: fs, Redis: rdb}, jwtKey: []byte(testJWTSecret)}
	login := func(code string) int {
		body, _ := json.Marshal(map[string]string{"username": "alice", "password": "pw", "totpCode": code})
		rec := httptest.NewRecorder()
		h.LoginHandler(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(string(body))))
		return rec.Code
	}

	// A right code first: the count it reserved is cleared again.
	for i := 0; i < 5; i++ {
		login("000000")
	}
	right, _ := totp.GenerateCode(secret, time.Now())
	if code := login(right); code != http.StatusOK {
		t.Fatalf("the right code inside the limit got %d", code)
	}
	if mr.Exists(wrongCodeKey("u1")) {
		t.Fatal("a successful login left the count standing")
	}

	// A burst: no more than the limit get to guess.
	codes := make(chan int, 3*wrongCodeLimit)
	var wg sync.WaitGroup
	for i := 0; i < 3*wrongCodeLimit; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- login("000000") }()
	}
	wg.Wait()
	close(codes)
	guessed := 0
	for c := range codes {
		if c == http.StatusUnauthorized {
			guessed++
		}
	}
	if guessed > wrongCodeLimit {
		t.Fatalf("%d parallel guesses were checked, limit %d", guessed, wrongCodeLimit)
	}
}
