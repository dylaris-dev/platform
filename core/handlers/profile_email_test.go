package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/services"

	"golang.org/x/crypto/bcrypt"
)

// profileFakeStore is the admin-email fake plus what the profile save touches.
type profileFakeStore struct {
	emailFakeStore
	passwordWrites []string
	mcWrites       []string
	renames        []string
}

func (f *profileFakeStore) GetUserByUsername(name string) (*models.User, error) {
	for _, u := range f.users {
		if u.Username == name {
			c := *u
			return &c, nil
		}
	}
	return nil, nil
}
func (f *profileFakeStore) UpdateUserPassword(id, hash string) error {
	f.passwordWrites = append(f.passwordWrites, hash)
	return nil
}
func (f *profileFakeStore) SetUserMinecraftUsername(id, mc string) error {
	f.mcWrites = append(f.mcWrites, mc)
	return nil
}
func (f *profileFakeStore) GetUserAccountPolicy() (bool, int, error) { return true, 0, nil }
func (f *profileFakeStore) UsernameTaken(string, string) (bool, error) {
	return false, nil
}
func (f *profileFakeStore) RenameUser(id, name, _ string) error {
	f.renames = append(f.renames, name)
	return nil
}

const profilePassword = "correct horse battery staple"

func newProfileStore(t *testing.T, verifyRequired bool) *profileFakeStore {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(profilePassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	me := &models.User{ID: "u-me", Username: "me", Email: "me@example.test", Password: string(hash)}
	other := &models.User{ID: "u-other", Username: "other", Email: "taken@example.test"}
	st := &profileFakeStore{emailFakeStore: emailFakeStore{
		users:    map[string]*models.User{me.ID: me, other.ID: other},
		byEmail:  map[string]*models.User{me.Email: me, other.Email: other},
		settings: map[string]string{},
	}}
	if verifyRequired {
		st.settings["auth.email_verify_required"] = "true"
	}
	return st
}

func saveProfile(t *testing.T, st *profileFakeStore, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body["oldPassword"] = profilePassword
	raw, _ := json.Marshal(body)
	h := NewAuthHandler(&AppState{Store: st}, "test-secret")
	r := httptest.NewRequest(http.MethodPut, "/api/auth/profile", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "username", "me"), "isAdmin", false))
	w := httptest.NewRecorder()
	h.UpdateProfileHandler(w, r)
	return w
}

// The account's own door changed an address straight through UpdateUser: no
// uniqueness check, the verified badge kept, no mail. The admin door has had
// all three, for a reason its own comment gives - the address is where a
// password reset goes. And because registration answers "account created" for
// an address that is already taken, claiming a stranger's address here meant
// that stranger could never register.
func TestTheProfileCannotClaimAnotherAccountsAddress(t *testing.T) {
	st := newProfileStore(t, true)
	w := saveProfile(t, st, map[string]interface{}{"email": "Taken@Example.test"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	if len(st.passwordWrites)+len(st.mcWrites) != 0 || len(st.setEmailCalls) != 0 {
		t.Error("the refused change was written anyway")
	}
}

// With verification required, a new address waits for its confirmation and
// the current one stays in force. Swapped at once, the account was locked out
// at its next sign-in by any typo, with the link on its way to a mailbox
// nobody reads.
func TestAProfileAddressWaitsForItsConfirmation(t *testing.T) {
	mails := captureAccountMail(t)
	st := newProfileStore(t, true)
	w := saveProfile(t, st, map[string]interface{}{"email": "New@Example.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(st.setEmailCalls) != 0 || st.users["u-me"].Email != "me@example.test" {
		t.Fatalf("the current address was replaced before the new one answered: %+v", st.setEmailCalls)
	}
	if st.pending["u-me"] != "new@example.test" {
		t.Fatalf("pending = %q, want the new address", st.pending["u-me"])
	}
	// Its own mail: the registration one welcomed a "new account", which a
	// stranger receiving it would read as a sign-up.
	if m := nextMail(t, mails); m.to != "new@example.test" || m.key != mailer.KeyConfirmEmailChange {
		t.Fatalf("the confirmation went to %q as %q", m.to, m.key)
	}
	var out map[string]string
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["pendingEmail"] != "new@example.test" || !strings.Contains(out["message"], "stays in use") {
		t.Fatalf("the answer does not say the address waits: %v", out)
	}
}

// A confirmation that never left leaves nothing pending behind it.
func TestAnUnsentConfirmationWithdrawsThePendingAddress(t *testing.T) {
	orig := sendAccountMail
	sendAccountMail = func(services.MailStore, string, string, string, map[string]string) error {
		return errors.New("relay refused")
	}
	t.Cleanup(func() { sendAccountMail = orig })
	st := newProfileStore(t, true)
	// The same save also changes the password and the name: refused whole, not
	// after half of it was written and the session already invalidated.
	w := saveProfile(t, st, map[string]interface{}{"email": "new@example.test", "newPassword": "another long password", "newUsername": "renamed"})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body.String())
	}
	if len(st.passwordWrites) != 0 || len(st.renames) != 0 {
		t.Fatalf("written before the mail failed: passwords %d, renames %v", len(st.passwordWrites), st.renames)
	}
	if st.pending["u-me"] != "" || st.users["u-me"].Email != "me@example.test" {
		t.Fatalf("pending %q, email %q after an unsent confirmation", st.pending["u-me"], st.users["u-me"].Email)
	}
}

// Without the policy nothing waits for a mail, and the address is the new one
// at once, unverified as when an admin types one.
func TestWithoutVerificationTheAddressChangesAtOnce(t *testing.T) {
	st := newProfileStore(t, false)
	w := saveProfile(t, st, map[string]interface{}{"email": "new@example.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(st.setEmailCalls) != 1 || st.setEmailCalls[0].email != "new@example.test" || len(st.pending) != 0 {
		t.Fatalf("setEmail %+v, pending %v", st.setEmailCalls, st.pending)
	}
}

// The link in that mail makes the waiting address the account's.
func TestTheConfirmationLinkSwapsInThePendingAddress(t *testing.T) {
	captureAccountMail(t)
	st := newProfileStore(t, true)
	st.pending = map[string]string{"u-me": "new@example.test"}
	me := *st.users["u-me"]
	me.PendingEmail = "new@example.test"
	st.byToken = &me
	w := verifyEmail(t, st)
	if w.Code != http.StatusOK || len(st.confirmed) != 1 || st.users["u-me"].Email != "new@example.test" {
		t.Fatalf("status %d, confirmed %v, email %q: %s", w.Code, st.confirmed, st.users["u-me"].Email, w.Body.String())
	}
}

// Somebody else may have taken the address while the mail waited.
func TestAPendingAddressTakenMeanwhileIsRefused(t *testing.T) {
	st := newProfileStore(t, true)
	st.pending = map[string]string{"u-me": "taken@example.test"}
	me := *st.users["u-me"]
	me.PendingEmail = "taken@example.test"
	st.byToken = &me
	w := verifyEmail(t, st)
	if w.Code != http.StatusConflict || len(st.confirmed) != 0 {
		t.Fatalf("status %d, confirmed %v", w.Code, st.confirmed)
	}
}

func verifyEmail(t *testing.T, st *profileFakeStore) *httptest.ResponseRecorder {
	t.Helper()
	h := NewRegistrationHandler(&AppState{Store: st})
	r := httptest.NewRequest(http.MethodPost, "/api/auth/verify-email", strings.NewReader(`{"token":"0123456789abcdef0123"}`))
	w := httptest.NewRecorder()
	h.VerifyEmail(w, r)
	return w
}

// Saving the form with the address it already shows must not un-verify it, or a
// stray save would lock an account out at its next sign-in.
func TestAnUnchangedAddressIsLeftAlone(t *testing.T) {
	st := newProfileStore(t, true)
	w := saveProfile(t, st, map[string]interface{}{"email": " ME@example.test "})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(st.setEmailCalls) != 0 {
		t.Errorf("an unchanged address was rewritten and un-verified: %+v", st.setEmailCalls)
	}
}

// Removing the address under a verify-required policy would lock the account
// out with nowhere to send the confirmation it then needs.
func TestTheAddressCannotBeRemovedWhenVerificationIsRequired(t *testing.T) {
	st := newProfileStore(t, true)
	if w := saveProfile(t, st, map[string]interface{}{"email": ""}); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
	// Without the policy it stays optional, as it always was.
	st2 := newProfileStore(t, false)
	if w := saveProfile(t, st2, map[string]interface{}{"email": ""}); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 when verification is off: %s", w.Code, w.Body.String())
	}
}

// The rename commits on its own. A refusal found after it would report failure
// for a save that had half happened - so the address is decided first.
func TestARefusedAddressDoesNotLeaveAHalfDoneRename(t *testing.T) {
	st := newProfileStore(t, true)
	w := saveProfile(t, st, map[string]interface{}{"newUsername": "renamed", "email": "taken@example.test"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	if len(st.renames) != 0 {
		t.Errorf("the rename went through although the save was refused: %v", st.renames)
	}
}

// Each address change mails the new address. Switching between two strangers'
// addresses in a loop sent them mail from this domain without end; one change
// per verification-mail window now.
func TestTheProfileCannotChangeTheAddressAgainWithinAMinute(t *testing.T) {
	st := newProfileStore(t, true)
	just := time.Now().Add(-10 * time.Second)
	st.users["u-me"].EmailVerificationSentAt = &just
	w := saveProfile(t, st, map[string]interface{}{"email": "fresh@example.test"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429: %s", w.Code, w.Body.String())
	}
	if len(st.setEmailCalls) != 0 {
		t.Fatal("the refused change was written anyway")
	}
}

// The profile save checks the current password on every call; a stolen
// session could guess it without limit, and an address change sends mail.
func TestTheProfileSaveIsRateLimited(t *testing.T) {
	src, err := os.ReadFile("../routes.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^.*HandleFunc\("/auth/profile".*UpdateProfileHandler.*$`).FindString(string(src))
	if m == "" || !strings.Contains(m, "authLimiter.Limit(") {
		t.Fatalf("PUT /auth/profile has no rate limiter: %q", m)
	}
}
