package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// totpFakeStore embeds store.Store (nil) so it satisfies the full interface at
// compile time; only the methods verifyTOTPOrBackup touches are overridden.
// Any other call would panic - these tests never make one. SetUserTOTP captures
// the destructive backup-code consumption write; InsertAuditIdentity is a no-op
// so the audit call on the consumption path does not reach the nil store.
type totpFakeStore struct {
	store.Store
	setCalls       int
	lastSetSecret  string
	lastSetBackups string
	lastSetEnabled bool
	setErr         error
	epochBumps     int
}

func (f *totpFakeStore) SetUserTOTP(id, secret, backupCodesJSON string, enabled bool) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls++
	f.lastSetSecret = secret
	f.lastSetBackups = backupCodesJSON
	f.lastSetEnabled = enabled
	return nil
}

// ConsumeTOTPBackupCode is the consumption write now; recorded the same way.
func (f *totpFakeStore) ConsumeTOTPBackupCode(id, readJSON, remainingJSON string) (bool, error) {
	if f.setErr != nil {
		return false, f.setErr
	}
	f.setCalls++
	f.lastSetBackups = remainingJSON
	f.lastSetEnabled = true // the write only lands while 2FA is on
	return true, nil
}

func (f *totpFakeStore) BumpSessionEpoch(string) (int, error) {
	f.epochBumps++
	return f.epochBumps, nil
}

func (f *totpFakeStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error { return nil }

func newTOTPTestAuthHandler(fs *totpFakeStore) *AuthHandler {
	state := &AppState{Store: fs}
	return NewAuthHandler(state, "test-jwt-secret")
}

// freshTOTPSecret returns a valid base32 TOTP secret.
func freshTOTPSecret(t *testing.T) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "a"})
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	return key.Secret()
}

// invalidTOTPCode returns a 6-digit code that does NOT validate against secret
// (checked against the current skew window), so the "reject" assertion cannot
// flake on an accidental match.
func invalidTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	for i := 0; i < 100000; i++ {
		c := fmt.Sprintf("%06d", i)
		if !totp.Validate(c, secret) {
			return c
		}
	}
	t.Fatalf("could not find an invalid TOTP code for secret")
	return ""
}

// bcryptBackups hashes each plain code (MinCost - fast, cost is irrelevant to a
// compare-only test) and returns the JSON array the store persists.
func bcryptBackups(t *testing.T, plain []string) string {
	t.Helper()
	hashed := make([]string, len(plain))
	for i, p := range plain {
		h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("hash backup code: %v", err)
		}
		hashed[i] = string(h)
	}
	b, err := json.Marshal(hashed)
	if err != nil {
		t.Fatalf("marshal backups: %v", err)
	}
	return string(b)
}

// TestVerifyTOTPOrBackup_TOTPPath covers the fast TOTP path plus the guard
// clauses for empty code / nil user. No backup codes are configured, so a
// non-matching code must fall through to a clean reject.
func TestVerifyTOTPOrBackup_TOTPPath(t *testing.T) {
	secret := freshTOTPSecret(t)
	validCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	cases := []struct {
		name    string
		user    *models.User
		code    string
		wantOK  bool
		wantErr bool
	}{
		{
			name:   "valid TOTP accepted",
			user:   &models.User{ID: "u1", TOTPSecret: secret},
			code:   validCode,
			wantOK: true,
		},
		{
			name:   "valid TOTP tolerates surrounding whitespace",
			user:   &models.User{ID: "u1", TOTPSecret: secret},
			code:   "  " + validCode + "  ",
			wantOK: true,
		},
		{
			name:   "invalid TOTP rejected (no backup codes)",
			user:   &models.User{ID: "u1", TOTPSecret: secret},
			code:   invalidTOTPCode(t, secret),
			wantOK: false,
		},
		{
			name:   "empty code rejected",
			user:   &models.User{ID: "u1", TOTPSecret: secret},
			code:   "   ",
			wantOK: false,
		},
		{
			name:   "nil user rejected",
			user:   nil,
			code:   validCode,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &totpFakeStore{}
			h := newTOTPTestAuthHandler(fs)
			ok, err := h.verifyTOTPOrBackup(tc.user, tc.code)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			// The TOTP path never persists; a matched-nothing path never persists.
			if fs.setCalls != 0 {
				t.Fatalf("SetUserTOTP called %d times on TOTP path, want 0", fs.setCalls)
			}
		})
	}
}

// TestVerifyTOTPOrBackup_BackupConsumption is the security-critical single-use
// property: a valid backup code is accepted exactly once, the matched code is
// dropped and the shrunken list persisted, and a second use of the same code is
// rejected. A wrong backup code is rejected without consuming anything.
func TestVerifyTOTPOrBackup_BackupConsumption(t *testing.T) {
	plain := []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"}
	backupsJSON := bcryptBackups(t, plain)

	// Empty TOTP secret forces the backup-code branch.
	user := &models.User{
		ID:              "u1",
		TOTPSecret:      "",
		TOTPBackupCodes: backupsJSON,
		Is2FAEnabled:    true,
	}

	fs := &totpFakeStore{}
	h := newTOTPTestAuthHandler(fs)

	// 1) A wrong backup code is rejected and consumes nothing.
	ok, err := h.verifyTOTPOrBackup(user, "not-a-real-backup-code")
	if err != nil || ok {
		t.Fatalf("wrong backup code: ok=%v err=%v, want false/nil", ok, err)
	}
	if fs.setCalls != 0 {
		t.Fatalf("wrong backup code persisted a change (setCalls=%d)", fs.setCalls)
	}

	// 2) A valid backup code (index 1) is accepted and consumed.
	ok, err = h.verifyTOTPOrBackup(user, plain[1])
	if err != nil || !ok {
		t.Fatalf("valid backup code: ok=%v err=%v, want true/nil", ok, err)
	}
	if fs.setCalls != 1 {
		t.Fatalf("expected exactly one persist on consumption, got %d", fs.setCalls)
	}
	if !fs.lastSetEnabled {
		t.Fatalf("consumption must preserve Is2FAEnabled=true, got false")
	}
	// The persisted list must be exactly the two unused codes, and must NOT
	// contain the consumed one.
	var remaining []string
	if err := json.Unmarshal([]byte(fs.lastSetBackups), &remaining); err != nil {
		t.Fatalf("persisted backups not valid JSON: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining backup count = %d, want 2", len(remaining))
	}
	for _, hstr := range remaining {
		if bcrypt.CompareHashAndPassword([]byte(hstr), []byte(plain[1])) == nil {
			t.Fatalf("consumed code %q still present in persisted list", plain[1])
		}
	}

	// 3) Reflect the persisted list back onto the user (as the real store would)
	// and re-submit the SAME code: it must now be rejected, and consume nothing
	// more.
	user.TOTPBackupCodes = fs.lastSetBackups
	ok, err = h.verifyTOTPOrBackup(user, plain[1])
	if err != nil || ok {
		t.Fatalf("reused backup code: ok=%v err=%v, want false/nil (single-use)", ok, err)
	}
	if fs.setCalls != 1 {
		t.Fatalf("reuse attempt triggered another persist (setCalls=%d)", fs.setCalls)
	}

	// 4) A still-unused code (index 0) remains valid after the reuse rejection.
	ok, err = h.verifyTOTPOrBackup(user, plain[0])
	if err != nil || !ok {
		t.Fatalf("remaining valid backup code: ok=%v err=%v, want true/nil", ok, err)
	}
	if fs.setCalls != 2 {
		t.Fatalf("expected second consumption to persist, setCalls=%d", fs.setCalls)
	}
}

// TestVerifyTOTPOrBackup_MalformedBackupJSON: a corrupt backup-codes column must
// surface as an error (fail closed), not a silent accept/reject, so the caller
// returns 500 rather than masking a broken account state.
func TestVerifyTOTPOrBackup_MalformedBackupJSON(t *testing.T) {
	user := &models.User{
		ID:              "u1",
		TOTPSecret:      "", // force the backup branch
		TOTPBackupCodes: "{not-a-json-array",
	}
	fs := &totpFakeStore{}
	h := newTOTPTestAuthHandler(fs)

	ok, err := h.verifyTOTPOrBackup(user, "some-code")
	if err == nil {
		t.Fatalf("expected an error on malformed backup JSON, got nil (ok=%v)", ok)
	}
	if ok {
		t.Fatalf("malformed backup JSON must not accept, got ok=true")
	}
	if fs.setCalls != 0 {
		t.Fatalf("malformed backup JSON persisted a change (setCalls=%d)", fs.setCalls)
	}
}

// TestVerifyTOTPOrBackup_PersistErrorPropagates: if the destructive consume
// write fails, the match must be reported as (false, err) so the code is NOT
// treated as accepted-and-consumed on a store failure.
func TestVerifyTOTPOrBackup_PersistErrorPropagates(t *testing.T) {
	plain := []string{"dddddddddddddddd"}
	user := &models.User{
		ID:              "u1",
		TOTPBackupCodes: bcryptBackups(t, plain),
		Is2FAEnabled:    true,
	}
	fs := &totpFakeStore{setErr: errors.New("db down")}
	h := newTOTPTestAuthHandler(fs)

	ok, err := h.verifyTOTPOrBackup(user, plain[0])
	if err == nil {
		t.Fatalf("expected persist error to propagate, got nil (ok=%v)", ok)
	}
	if ok {
		t.Fatalf("persist failure must not report acceptance, got ok=true")
	}
}

// postJSONAs runs an authed handler directly, with the username the auth
// middleware would have put in the context.
func postJSONAs(t *testing.T, h http.HandlerFunc, username string, body interface{}) *httptest.ResponseRecorder {
	return postJSONAsPurpose(t, h, username, "", body)
}

// postJSONAsPurpose is the same, with the JWT purpose the middleware would have
// attached ("" = a normal session, "2fa_setup" = a fresh password login).
func postJSONAsPurpose(t *testing.T, h http.HandlerFunc, username, purpose string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
	ctx := context.WithValue(req.Context(), "username", username) //nolint:staticcheck // matches the middleware's key
	ctx = context.WithValue(ctx, tokenPurposeKey, purpose)
	rr := httptest.NewRecorder()
	h(rr, req.WithContext(ctx))
	return rr
}

// enrolFakeStore serves one user to VerifyTOTPHandler and records the enable.
type enrolFakeStore struct {
	totpFakeStore
	user *models.User
}

func (f *enrolFakeStore) GetUserByUsername(string) (*models.User, error) { return f.user, nil }

// Enrolment is the step that decides who holds the second factor from then on,
// so it must re-authenticate. Without the password a stolen session alone could
// bind the ATTACKER's authenticator to the account, take the one-time backup
// codes and lock the owner out - a temporary session compromise turned into
// durable takeover, using the security feature as the lock. Disable and
// RegenerateBackupCodes always asked; enrolment did not.
func TestVerifyTOTPRequiresPassword(t *testing.T) {
	const password = "correct-horse-battery"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	secret := freshTOTPSecret(t)

	newStore := func() *enrolFakeStore {
		return &enrolFakeStore{user: &models.User{
			ID: "u1", Username: "alice", Password: string(hash),
		}}
	}
	validCode := func(t *testing.T) string {
		t.Helper()
		code, cerr := totp.GenerateCode(secret, time.Now())
		if cerr != nil {
			t.Fatalf("generate code: %v", cerr)
		}
		return code
	}

	t.Run("a valid code without the password does not enable 2FA", func(t *testing.T) {
		fs := newStore()
		h := newTOTPTestAuthHandler(&fs.totpFakeStore)
		h.state.Store = fs
		body := VerifyTOTPRequest{Secret: secret, Code: validCode(t)}
		rr := postJSONAs(t, h.VerifyTOTPHandler, "alice", body)
		// 403: the session is valid, the credential is not; a 401 signs the
		// panel out.
		if rr.Code != 403 {
			t.Errorf("status = %d, want 403", rr.Code)
		}
		if fs.setCalls != 0 {
			t.Error("2FA was enabled without the account password")
		}
	})

	t.Run("a wrong password does not enable 2FA", func(t *testing.T) {
		fs := newStore()
		h := newTOTPTestAuthHandler(&fs.totpFakeStore)
		h.state.Store = fs
		body := VerifyTOTPRequest{Secret: secret, Code: validCode(t), Password: "wrong"}
		rr := postJSONAs(t, h.VerifyTOTPHandler, "alice", body)
		if rr.Code != 403 || fs.setCalls != 0 {
			t.Errorf("status = %d, setCalls = %d; want 403 and no write", rr.Code, fs.setCalls)
		}
	})

	t.Run("the right password plus a valid code enables it", func(t *testing.T) {
		fs := newStore()
		h := newTOTPTestAuthHandler(&fs.totpFakeStore)
		h.state.Store = fs
		body := VerifyTOTPRequest{Secret: secret, Code: validCode(t), Password: password}
		rr := postJSONAs(t, h.VerifyTOTPHandler, "alice", body)
		if rr.Code != 200 {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
		if fs.setCalls != 1 || !fs.lastSetEnabled || fs.lastSetSecret != secret {
			t.Errorf("enable not persisted as expected: calls=%d enabled=%v secret=%q",
				fs.setCalls, fs.lastSetEnabled, fs.lastSetSecret)
		}
	})

	// The password must not be checked before the code: a caller who only knows
	// the password should not learn it is right by watching the error change.
	t.Run("a bad code with the right password still fails", func(t *testing.T) {
		fs := newStore()
		h := newTOTPTestAuthHandler(&fs.totpFakeStore)
		h.state.Store = fs
		body := VerifyTOTPRequest{Secret: secret, Code: invalidTOTPCode(t, secret), Password: password}
		rr := postJSONAs(t, h.VerifyTOTPHandler, "alice", body)
		if rr.Code == 200 || fs.setCalls != 0 {
			t.Errorf("status = %d, setCalls = %d; want a rejection", rr.Code, fs.setCalls)
		}
	})
}

// The forced-enrolment flow must not be blocked by the new password prompt. Its
// token is minted by the login handler only after a successful password check,
// expires in 15 minutes and reaches three paths, so the password is already
// proven - asking again would demand something the user answered seconds ago.
func TestVerifyTOTPSetupTokenSkipsPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	secret := freshTOTPSecret(t)
	code, cerr := totp.GenerateCode(secret, time.Now())
	if cerr != nil {
		t.Fatalf("code: %v", cerr)
	}

	fs := &enrolFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: string(hash)}}
	h := newTOTPTestAuthHandler(&fs.totpFakeStore)
	h.state.Store = fs

	rr := postJSONAsPurpose(t, h.VerifyTOTPHandler, "alice", "2fa_setup",
		VerifyTOTPRequest{Secret: secret, Code: code})
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if fs.setCalls != 1 || !fs.lastSetEnabled {
		t.Errorf("forced enrolment did not enable 2FA: calls=%d enabled=%v", fs.setCalls, fs.lastSetEnabled)
	}
}
