package handlers

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/handlers/reauth"
	"dylaris-core/models"
	"dylaris-core/store"
)

// reauthFakeStore is the smallest store requireReauth touches: one user lookup,
// plus SetUserTOTP for the branch that consumes a backup code.
type reauthFakeStore struct {
	store.Store
	users     map[string]*models.User
	lookupErr error
	totpSaved bool
}

func (f *reauthFakeStore) GetUserByID(id string) (*models.User, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	u, ok := f.users[id]
	if !ok {
		return nil, errors.New("user not found")
	}
	return u, nil
}

func (f *reauthFakeStore) SetUserTOTP(id, secret, backupCodes string, enabled bool) error {
	f.totpSaved = true
	if u, ok := f.users[id]; ok {
		u.TOTPBackupCodes = backupCodes
	}
	return nil
}

// Consuming a backup code audits the consumption, so the fake has to answer
// that call - the embedded nil store.Store would panic instead.
// ConsumeTOTPBackupCode behaves like the store: it writes only over the list
// the caller read.
func (f *reauthFakeStore) ConsumeTOTPBackupCode(id, readJSON, remainingJSON string) (bool, error) {
	u, ok := f.users[id]
	if !ok || !u.Is2FAEnabled || u.TOTPBackupCodes != readJSON {
		return false, nil
	}
	f.totpSaved = true
	u.TOTPBackupCodes = remainingJSON
	return true, nil
}

func (f *reauthFakeStore) InsertAuditIdentity(e *models.AuditEventIdentity) error { return nil }

func hashFor(t *testing.T, plain string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(plain), 4)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return string(h)
}

func reauthState(t *testing.T, u *models.User) (*AppState, *reauthFakeStore) {
	t.Helper()
	fs := &reauthFakeStore{users: map[string]*models.User{u.ID: u}}
	return &AppState{Store: fs}, fs
}

// An account WITHOUT 2FA has no second factor to present. Demanding a code
// anyway would not harden anything - it would lock every such user out of the
// action completely, which is how a guard in this repo has silently covered
// nobody before. So this pins that the carve-out actually lets them through,
// and that it still costs them the password.
func TestReauthWithoutTwoFactorTakesThePasswordAlone(t *testing.T) {
	u := &models.User{ID: "u1", Password: hashFor(t, "correct horse"), Is2FAEnabled: false}
	state, _ := reauthState(t, u)

	if err := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "correct horse", ""); err != nil {
		t.Errorf("a user without 2FA cannot re-authenticate at all: %v", err)
	}
	if err := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "wrong", ""); err == nil {
		t.Error("the wrong password was accepted, so the carve-out skipped the whole check")
	}
}

// With 2FA on, the password alone must not be enough - otherwise the second
// factor is decorative on exactly the actions that outlive a password change.
func TestReauthWithTwoFactorNeedsBoth(t *testing.T) {
	u := &models.User{
		ID: "u1", Password: hashFor(t, "correct horse"),
		Is2FAEnabled: true, TOTPSecret: "JBSWY3DPEHPK3PXP",
	}
	state, _ := reauthState(t, u)

	for name, code := range map[string]string{"empty": "", "wrong": "000000"} {
		t.Run(name+" code", func(t *testing.T) {
			err := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "correct horse", code)
			if err == nil {
				t.Fatal("the password alone was accepted while 2FA is on")
			}
			rec := httptest.NewRecorder()
			reauth.WriteReauthError(rec, err)
			if rec.Code != 401 {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}

	// The wrong password must be refused before the code is even considered,
	// so a valid code can never carry a caller who does not know the password.
	if err := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "wrong", "000000"); err == nil {
		t.Error("the wrong password was accepted")
	}
}

// A user whose phone is gone still has to be able to manage their account, so
// backup codes count here - unlike the ticket-restore Danger Zone, which is
// destructive rather than administrative and refuses them on purpose.
func TestReauthAcceptsABackupCodeAndConsumesIt(t *testing.T) {
	code := "abcdef0123456789"
	hashed, err := bcrypt.GenerateFromPassword([]byte(code), 4)
	if err != nil {
		t.Fatal(err)
	}
	u := &models.User{
		ID: "u1", Password: hashFor(t, "correct horse"),
		Is2FAEnabled: true, TOTPSecret: "JBSWY3DPEHPK3PXP",
		TOTPBackupCodes: `["` + string(hashed) + `"]`,
	}
	state, fs := reauthState(t, u)

	if rerr := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "correct horse", code); rerr != nil {
		t.Fatalf("a valid backup code was refused: %v", rerr)
	}
	if !fs.totpSaved {
		t.Error("the backup code was not written back, so it can be replayed")
	}
	if rerr := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "correct horse", code); rerr == nil {
		t.Error("the same backup code worked twice")
	}
}

// A store that cannot answer must not read as a successful re-authentication.
func TestReauthFailsClosedWhenTheUserCannotBeLoaded(t *testing.T) {
	state := &AppState{Store: &reauthFakeStore{lookupErr: errors.New("db down")}}
	if err := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "anything", ""); err == nil {
		t.Fatal("a failed user lookup was treated as a successful re-authentication")
	}
	if err := reauth.RequireReauth(nil, nil, "u1", "anything", ""); err == nil {
		t.Fatal("a nil state was treated as a successful re-authentication")
	}
}

// One prompt covers the two writes a single save makes (the panel's "role and
// permissions" button calls two endpoints), so re-authentication checks a
// TOTP code without spending its step; the login spends it. Pinned with a
// real replay store, because without Redis every step reads as free and the
// difference cannot show. The check is wired through ReauthCodeVerifier since
// re-authentication moved to handlers/reauth.
func TestOneReauthCodeCoversTwoWritesButNotTwoLogins(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	u := &models.User{ID: "u1", Password: hashFor(t, "correct horse"), Is2FAEnabled: true, TOTPSecret: secret}
	state, _ := reauthState(t, u)
	state.Redis, _ = handlertest.Redis(t)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if rerr := reauth.RequireReauth(state.Store, ReauthCodeVerifier(state), "u1", "correct horse", code); rerr != nil {
			t.Fatalf("re-authentication %d refused the code: %v", i+1, rerr)
		}
	}
	if ok, err := verifyTOTPOrBackupFor(state, u, code); !ok || err != nil {
		t.Fatalf("login refused a code re-authentication had only checked: %v %v", ok, err)
	}
	if ok, _ := verifyTOTPOrBackupFor(state, u, code); ok {
		t.Fatal("the login accepted the same code twice")
	}
}
