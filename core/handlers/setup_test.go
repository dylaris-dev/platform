package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// setupFakeStore embeds store.Store (nil) so it satisfies the full interface at
// compile time; only the methods CreateAdmin touches are overridden. Any other
// call would panic - the tests never make one.
type setupFakeStore struct {
	store.Store
	userCount  int
	adminCount int
	countErr   error
	createErr  error
	created    []string
	codesFor   string
	codesJSON  string
}

func (f *setupFakeStore) SetUserTOTPBackupCodes(id, backupCodesJSON string) error {
	f.codesFor, f.codesJSON = id, backupCodesJSON
	return nil
}

func (f *setupFakeStore) CountUsers() (int, error)  { return f.userCount, f.countErr }
func (f *setupFakeStore) CountAdmins() (int, error) { return f.adminCount, f.countErr }

func (f *setupFakeStore) CreateFirstAdmin(username, passwordHash, totpSecret string) (*models.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, username)
	return &models.User{ID: "id-" + username, Username: username, IsAdmin: true, Role: "admin"}, nil
}

func (f *setupFakeStore) CreateAdditionalAdmin(username, passwordHash, totpSecret string) (*models.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, username)
	return &models.User{ID: "id2-" + username, Username: username, IsAdmin: true, Role: "admin"}, nil
}

func newSetupTestHandler(fs *setupFakeStore, adminSecret string) *SetupHandler {
	return newSetupTestHandlerWithSetup(fs, adminSecret, false)
}

// newSetupTestHandlerWithSetup builds a handler with env SETUP set explicitly.
// The default is FALSE, matching production: on an instance that already has an
// admin, /setup is shut unless the operator opened it.
func newSetupTestHandlerWithSetup(fs *setupFakeStore, adminSecret string, setupEnabled bool) *SetupHandler {
	state := &AppState{Store: fs, AdminSecret: adminSecret, SetupEnabled: setupEnabled, FrontendURL: "https://panel.example.com"}
	auth := NewAuthHandler(state, "test-jwt-secret")
	return NewSetupHandler(state, auth)
}

func postCreateAdmin(h *SetupHandler, body map[string]interface{}) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/setup/admin", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.CreateAdmin(rec, req)
	return rec
}

func TestSetupStatus_ReportsAdminSecretConfigured(t *testing.T) {
	fs := &setupFakeStore{userCount: 0, adminCount: 0}
	h := newSetupTestHandler(fs, "correct-horse-battery-staple")
	req := httptest.NewRequest("GET", "/api/setup/status", nil)
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	var out struct {
		Mode                  string `json:"mode"`
		AdminSecretConfigured bool   `json:"adminSecretConfigured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Mode != "fresh_install" || !out.AdminSecretConfigured {
		t.Fatalf("status = %+v, want fresh_install + configured", out)
	}
	if strings.Contains(rec.Body.String(), "correct-horse-battery-staple") {
		t.Fatalf("secret leaked into status body: %s", rec.Body.String())
	}
}

func TestCreateAdmin_Matrix(t *testing.T) {
	const secret = "correct-horse-battery-staple"

	cases := []struct {
		name        string
		userCount   int
		adminCount  int
		adminSecret string // configured on the server
		// setupEnabled is env SETUP. Only consulted once an admin exists, so
		// every fresh-install and lost-admin case below leaves it false on
		// purpose: those must work without it.
		setupEnabled bool
		body         map[string]interface{}
		createErr    error
		wantStatus   int
		wantErrCode  string // "" means success
	}{
		{
			name:       "fresh install unset -> 200",
			body:       map[string]interface{}{"username": "alice", "password": "password123"},
			wantStatus: http.StatusOK,
		},
		{
			name:        "fresh install set + correct -> 200",
			adminSecret: secret,
			body:        map[string]interface{}{"username": "alice", "password": "password123", "adminSecret": secret},
			wantStatus:  http.StatusOK,
		},
		{
			name:        "fresh install set + wrong -> 403 invalid_admin_secret",
			adminSecret: secret,
			body:        map[string]interface{}{"username": "alice", "password": "password123", "adminSecret": "nope"},
			wantStatus:  http.StatusForbidden, wantErrCode: "invalid_admin_secret",
		},
		{
			name:        "fresh install set + missing -> 403 invalid_admin_secret",
			adminSecret: secret,
			body:        map[string]interface{}{"username": "alice", "password": "password123"},
			wantStatus:  http.StatusForbidden, wantErrCode: "invalid_admin_secret",
		},
		{
			name:       "lost admin unset -> 403 admin_recovery_disabled",
			userCount:  3,
			body:       map[string]interface{}{"username": "alice", "password": "password123"},
			wantStatus: http.StatusForbidden, wantErrCode: "admin_recovery_disabled",
		},
		{
			name:        "lost admin set + correct -> 200",
			userCount:   3,
			adminSecret: secret,
			body:        map[string]interface{}{"username": "alice", "password": "password123", "adminSecret": secret},
			wantStatus:  http.StatusOK,
		},
		{
			// SETUP is checked before the secret, so the reason reported is the
			// door being shut rather than a missing secret. That ordering is the
			// point: an operator who turned setup off should not be sent hunting
			// for an env var that would not have helped.
			name:       "complete unset + SETUP off -> 403 setup_disabled",
			userCount:  1,
			adminCount: 1,
			body:       map[string]interface{}{"username": "backup", "password": "password123"},
			wantStatus: http.StatusForbidden, wantErrCode: "setup_disabled",
		},
		{
			// The case env SETUP was added for: a correct secret is not enough on
			// a live instance, because the operator switched the door off.
			name:        "complete set + correct but SETUP off -> 403 setup_disabled",
			userCount:   1,
			adminCount:  1,
			adminSecret: secret,
			body:        map[string]interface{}{"username": "backup", "password": "password123", "adminSecret": secret},
			wantStatus:  http.StatusForbidden, wantErrCode: "setup_disabled",
		},
		{
			name:         "complete set + correct + SETUP on -> 200 (break-glass additional admin)",
			userCount:    1,
			adminCount:   1,
			adminSecret:  secret,
			setupEnabled: true,
			body:         map[string]interface{}{"username": "backup", "password": "password123", "adminSecret": secret},
			wantStatus:   http.StatusOK,
		},
		{
			name:         "complete set + wrong + SETUP on -> 403 invalid_admin_secret",
			userCount:    1,
			adminCount:   1,
			adminSecret:  secret,
			setupEnabled: true,
			body:         map[string]interface{}{"username": "backup", "password": "password123", "adminSecret": "nope"},
			wantStatus:   http.StatusForbidden, wantErrCode: "invalid_admin_secret",
		},
		{
			// SETUP on with no secret configured is a misconfiguration, not a way
			// in. The status endpoint answers it with the red banner.
			name:         "complete SETUP on but no secret -> 403 admin_recovery_disabled",
			userCount:    1,
			adminCount:   1,
			setupEnabled: true,
			body:         map[string]interface{}{"username": "backup", "password": "password123"},
			wantStatus:   http.StatusForbidden, wantErrCode: "admin_recovery_disabled",
		},
		{
			name:       "allowed but bad username -> 400 invalid_username",
			body:       map[string]interface{}{"username": "!!", "password": "password123"},
			wantStatus: http.StatusBadRequest, wantErrCode: "invalid_username",
		},
		{
			name:       "allowed but short password -> 400 invalid_password",
			body:       map[string]interface{}{"username": "alice", "password": "short"},
			wantStatus: http.StatusBadRequest, wantErrCode: "invalid_password",
		},
		{
			name:       "first-admin race lost -> 409 setup_already_complete",
			body:       map[string]interface{}{"username": "alice", "password": "password123"},
			createErr:  store.ErrSetupAlreadyComplete,
			wantStatus: http.StatusConflict, wantErrCode: "setup_already_complete",
		},
		{
			name:         "break-glass username collision -> 409 username_taken",
			userCount:    1,
			adminCount:   1,
			adminSecret:  secret,
			setupEnabled: true,
			body:         map[string]interface{}{"username": "backup", "password": "password123", "adminSecret": secret},
			createErr:    store.ErrUsernameTaken,
			wantStatus:   http.StatusConflict, wantErrCode: "username_taken",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &setupFakeStore{userCount: tc.userCount, adminCount: tc.adminCount, createErr: tc.createErr}
			h := newSetupTestHandlerWithSetup(fs, tc.adminSecret, tc.setupEnabled)
			rec := postCreateAdmin(h, tc.body)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			var out struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
				Token   string `json:"token"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if tc.wantErrCode == "" {
				if !out.Success || out.Token == "" {
					t.Fatalf("expected success+token, got %s", rec.Body.String())
				}
			} else if out.Error != tc.wantErrCode {
				t.Fatalf("error = %q, want %q: %s", out.Error, tc.wantErrCode, rec.Body.String())
			}

			// The configured secret must NEVER appear in any response body.
			if tc.adminSecret != "" && strings.Contains(rec.Body.String(), tc.adminSecret) {
				t.Fatalf("secret leaked into response body: %s", rec.Body.String())
			}
		})
	}
}

// TestCreateAdmin_CountErrorFailsClosed: a transient DB error on either count must
// return 500 before the adminCreateAllowed gate ever runs. Without this, a mis-read
// count (userCount=0 instead of the real 3) would make a real lost_admin system with
// no ADMIN_SECRET configured look like a fresh install, reopening secret-less admin
// creation - exactly what adminCreateAllowed(0, "", "") == true would allow.
func TestCreateAdmin_CountErrorFailsClosed(t *testing.T) {
	fs := &setupFakeStore{userCount: 3, adminCount: 0, countErr: errors.New("db down")}
	h := newSetupTestHandler(fs, "") // no ADMIN_SECRET configured
	body := map[string]interface{}{"username": "alice", "password": "password123"}

	rec := postCreateAdmin(h, body)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	var out struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Success || out.Error != "count_failed" {
		t.Fatalf("error = %q (success=%v), want count_failed: %s", out.Error, out.Success, rec.Body.String())
	}
	if len(fs.created) != 0 {
		t.Fatalf("expected no user created, got %v", fs.created)
	}
}

// Creating the first admin has to SIGN THEM IN, and for a while it did not.
//
// It issues a token and returns it in the body, which was the whole mechanism
// while the panel kept a token in localStorage. The session is an HttpOnly
// cookie now, and this endpoint was the one place that issues a session and was
// never taught to set one - so the wizard finished, sent the new operator to
// /servers, and the authed layout bounced them straight to /login to type the
// password they had chosen ten seconds earlier.
//
// The panel even carried a comment saying it was signed in "through the same
// HttpOnly cookie a normal login sets". Nothing set it.
func TestCreateAdmin_SignsTheNewAdminIn(t *testing.T) {
	fs := &setupFakeStore{}
	h := newSetupTestHandlerWithSetup(fs, "", false)
	rec := postCreateAdmin(h, map[string]interface{}{"username": "alice", "password": "password123"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	c := cookieByName(rec, sessionCookieName)
	if c == nil {
		t.Fatal("no session cookie: the new admin has to log in again immediately")
	}
	if !c.HttpOnly {
		t.Error("the session cookie is readable by script")
	}
	if c.Value == "" {
		t.Error("the session cookie is empty")
	}
	// The readable companion, or the panel renders the login screen anyway.
	if h := cookieByName(rec, signedInHintName); h == nil {
		t.Error("no signed-in hint; the panel cannot tell it is signed in")
	}

	// And it must be the SAME session the body advertises - two different ones
	// would mean the cookie belongs to a session nothing else knows about.
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Token != "" && out.Token != c.Value {
		t.Error("the cookie carries a different session than the response body")
	}
}

// A REFUSED setup must not hand out a session. Obvious, and worth pinning:
// the cookie is set near the end of a handler with several early returns, and
// one of them landing after it would be a free admin session.
func TestCreateAdmin_RefusalSetsNoCookie(t *testing.T) {
	fs := &setupFakeStore{userCount: 3}
	h := newSetupTestHandlerWithSetup(fs, "", false)
	rec := postCreateAdmin(h, map[string]interface{}{"username": "alice", "password": "password123"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if c := cookieByName(rec, sessionCookieName); c != nil {
		t.Fatalf("a refused setup set a session cookie: %q", c.Value)
	}
}

// The wizard turns 2FA on; the admin got no backup codes, so a lost phone left
// the only admin to ADMIN_SECRET break-glass. The codes come back once, and
// what is stored is their hashes.
func TestCreateAdmin_With2FAHandsOutBackupCodes(t *testing.T) {
	fs := &setupFakeStore{}
	h := newSetupTestHandlerWithSetup(fs, "", false)
	secret := "JBSWY3DPEHPK3PXP"
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rec := postCreateAdmin(h, map[string]interface{}{
		"username": "alice", "password": "password123",
		"totp": map[string]string{"secret": secret, "code": code},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		BackupCodes []string `json:"backupCodes"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.BackupCodes) != backupCodeCount {
		t.Fatalf("got %d backup codes, want %d", len(out.BackupCodes), backupCodeCount)
	}
	var hashed []string
	if err := json.Unmarshal([]byte(fs.codesJSON), &hashed); err != nil || fs.codesFor != "id-alice" || len(hashed) != backupCodeCount {
		t.Fatalf("stored %q for %q", fs.codesJSON, fs.codesFor)
	}
	if bcrypt.CompareHashAndPassword([]byte(hashed[0]), []byte(out.BackupCodes[0])) != nil {
		t.Error("the stored hash does not match the code handed out")
	}
	if strings.Contains(fs.codesJSON, out.BackupCodes[0]) {
		t.Error("a backup code was stored in the clear")
	}

	// Without 2FA there is nothing to hand out.
	fs2 := &setupFakeStore{}
	rec = postCreateAdmin(newSetupTestHandlerWithSetup(fs2, "", false), map[string]interface{}{"username": "bob", "password": "password123"})
	if strings.Contains(rec.Body.String(), "backupCodes") || fs2.codesJSON != "" {
		t.Errorf("codes for an admin without 2FA: %s", rec.Body.String())
	}
}

// A response carrying the backup codes (and a session token) must not be kept
// by any cache.
func TestBackupCodeResponsesAreNotCached(t *testing.T) {
	for _, fn := range []struct{ file, name string }{
		{"setup.go", "CreateAdmin"},
		{"totp.go", "VerifyTOTPHandler"},
		{"totp.go", "RegenerateBackupCodesHandler"},
	} {
		if !strings.Contains(funcBody(t, fn.file, fn.name), `"Cache-Control", "no-store"`) {
			t.Errorf("%s sends backup codes without no-store", fn.name)
		}
	}
}
