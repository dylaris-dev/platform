package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"

	"golang.org/x/crypto/bcrypt"
)

// profileFakeStore is the admin-email fake plus what the profile save touches.
type profileFakeStore struct {
	emailFakeStore
	updates []models.User
	renames []string
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
func (f *profileFakeStore) UpdateUser(u *models.User) error {
	f.updates = append(f.updates, *u)
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
	if len(st.updates) != 0 || len(st.setEmailCalls) != 0 {
		t.Error("the refused change was written anyway")
	}
}

// A new address is unproven, exactly as when an admin types one.
func TestAChangedAddressLosesItsVerifiedBadge(t *testing.T) {
	st := newProfileStore(t, true)
	w := saveProfile(t, st, map[string]interface{}{"email": "new@example.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(st.setEmailCalls) != 1 || st.setEmailCalls[0].email != "new@example.test" {
		t.Fatalf("the address was not stored through the un-verifying write: %+v", st.setEmailCalls)
	}
	if st.tokenSetFor != "u-me" {
		t.Error("no confirmation was issued for the new address although the policy requires one")
	}
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
