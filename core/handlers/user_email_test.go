package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type emailFakeStore struct {
	store.Store
	users    map[string]*models.User
	byEmail  map[string]*models.User
	settings map[string]string

	setEmailCalls []struct{ id, email string }
	tokenSetFor   string
}

// The mail dispatcher asks for an operator override before rendering. A store
// that embeds the interface answers nil-pointer here rather than "no row",
// so the fake has to say it explicitly: this install has edited nothing.
func (f *emailFakeStore) GetMailTemplate(string) (*models.MailTemplate, error) {
	return nil, nil
}

func (f *emailFakeStore) GetUserByID(id string) (*models.User, error) { return f.users[id], nil }
func (f *emailFakeStore) GetUserByEmail(e string) (*models.User, error) {
	return f.byEmail[e], nil
}
func (f *emailFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *emailFakeStore) SetUserEmail(id, email string) error {
	f.setEmailCalls = append(f.setEmailCalls, struct{ id, email string }{id, email})
	if u := f.users[id]; u != nil {
		u.Email = email
	}
	return nil
}
func (f *emailFakeStore) SetEmailVerificationToken(id, _ string) error {
	f.tokenSetFor = id
	return nil
}
func (f *emailFakeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

const emailTestUserID = "11111111-1111-1111-1111-111111111111"

func emailReq(t *testing.T, st *emailFakeStore, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	h := NewUserEmailHandler(&AppState{Store: st})
	r := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+emailTestUserID+"/email", bytes.NewReader([]byte(body)))
	r = mux.SetURLVars(r, map[string]string{"id": emailTestUserID})
	r = r.WithContext(context.WithValue(r.Context(), "userID", "admin-1"))
	w := httptest.NewRecorder()
	h.SetEmail(w, r)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func emailStore() *emailFakeStore {
	u := &models.User{ID: emailTestUserID, Username: "cust", Email: "old@example.com"}
	return &emailFakeStore{
		users:    map[string]*models.User{emailTestUserID: u},
		byEmail:  map[string]*models.User{"old@example.com": u},
		settings: map[string]string{},
	}
}

// The hole this fills: an account whose address is wrong could be renamed but
// never reached, and while security questions are off the reset link is the only
// way back in.
func TestAnAdminCanChangeAnAccountsEmail(t *testing.T) {
	st := emailStore()
	w, out := emailReq(t, st, `{"email":"  New@Example.COM  "}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(st.setEmailCalls) != 1 {
		t.Fatalf("SetUserEmail called %d times, want 1", len(st.setEmailCalls))
	}
	// Normalised, so two accounts cannot differ only by case or whitespace.
	if got := st.setEmailCalls[0].email; got != "new@example.com" {
		t.Errorf("stored %q, want the trimmed lowercase address", got)
	}
	if out["email"] != "new@example.com" {
		t.Errorf("reply email = %v", out["email"])
	}
}

// Storing the same address again would clear email_verified_at, so a stray save
// on a screen that shows the current address would un-verify a healthy account -
// and lock its owner out while the policy is on.
func TestSavingTheSameAddressChangesNothing(t *testing.T) {
	st := emailStore()
	_, out := emailReq(t, st, `{"email":"OLD@example.com"}`)

	if len(st.setEmailCalls) != 0 {
		t.Fatalf("the address was rewritten though it did not change: %+v", st.setEmailCalls)
	}
	if out["unchanged"] != true {
		t.Errorf("unchanged = %v, want true", out["unchanged"])
	}
}

// users.email has no unique index, so this check is the only thing standing
// between two accounts and the same reset mailbox.
func TestAnAddressAlreadyInUseIsRefused(t *testing.T) {
	st := emailStore()
	other := &models.User{ID: "22222222-2222-2222-2222-222222222222", Email: "taken@example.com"}
	st.byEmail["taken@example.com"] = other

	w, _ := emailReq(t, st, `{"email":"taken@example.com"}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if len(st.setEmailCalls) != 0 {
		t.Error("the address was stored anyway")
	}
}

func TestAnInvalidAddressIsRefused(t *testing.T) {
	for _, bad := range []string{`{"email":""}`, `{"email":"nope"}`, `{"email":"a@b"}`, `{"email":"a b@c.de"}`} {
		st := emailStore()
		w, _ := emailReq(t, st, bad)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s -> status %d, want 400", bad, w.Code)
		}
		if len(st.setEmailCalls) != 0 {
			t.Errorf("%s was stored", bad)
		}
	}
}

// A verification is issued only when the policy demands one. Without it the
// account is usable immediately and the mail would be noise; with it, the
// account cannot be used until the new mailbox answers - which is the point.
func TestAVerificationIsIssuedOnlyWhenThePolicyRequiresIt(t *testing.T) {
	st := emailStore()
	emailReq(t, st, `{"email":"new@example.com"}`)
	if st.tokenSetFor != "" {
		t.Error("a verification token was issued although verification is off")
	}

	st = emailStore()
	st.settings["auth.email_verify_required"] = "true"
	_, out := emailReq(t, st, `{"email":"new@example.com"}`)
	if st.tokenSetFor != emailTestUserID {
		t.Error("no verification token was issued although the policy requires one")
	}
	if out["emailVerifySent"] != false {
		// No transport is configured in this fake, so the send fails and the
		// reply must say so rather than claiming a mail went out.
		t.Errorf("emailVerifySent = %v; an unsent mail must not be reported as sent", out["emailVerifySent"])
	}
}
