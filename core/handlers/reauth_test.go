package handlers

import (
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"

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

	if err := requireReauth(state, "u1", "correct horse", ""); err != nil {
		t.Errorf("a user without 2FA cannot re-authenticate at all: %v", err)
	}
	if err := requireReauth(state, "u1", "wrong", ""); err == nil {
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
			err := requireReauth(state, "u1", "correct horse", code)
			if err == nil {
				t.Fatal("the password alone was accepted while 2FA is on")
			}
			if err.status != 401 {
				t.Errorf("status = %d, want 401", err.status)
			}
		})
	}

	// The wrong password must be refused before the code is even considered,
	// so a valid code can never carry a caller who does not know the password.
	if err := requireReauth(state, "u1", "wrong", "000000"); err == nil {
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

	if rerr := requireReauth(state, "u1", "correct horse", code); rerr != nil {
		t.Fatalf("a valid backup code was refused: %v", rerr)
	}
	if !fs.totpSaved {
		t.Error("the backup code was not written back, so it can be replayed")
	}
	if rerr := requireReauth(state, "u1", "correct horse", code); rerr == nil {
		t.Error("the same backup code worked twice")
	}
}

// A store that cannot answer must not read as a successful re-authentication.
func TestReauthFailsClosedWhenTheUserCannotBeLoaded(t *testing.T) {
	state := &AppState{Store: &reauthFakeStore{lookupErr: errors.New("db down")}}
	if err := requireReauth(state, "u1", "anything", ""); err == nil {
		t.Fatal("a failed user lookup was treated as a successful re-authentication")
	}
	if err := requireReauth(nil, "u1", "anything", ""); err == nil {
		t.Fatal("a nil state was treated as a successful re-authentication")
	}
}
