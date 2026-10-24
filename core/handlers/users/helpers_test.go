package users

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of handlers test fixtures the user tests use; each keeps its name.

const (
	guardStaff    = "10000000-0000-4000-8000-000000000001" // holds users.write
	guardAdmin    = "10000000-0000-4000-8000-000000000002"
	guardMember   = "10000000-0000-4000-8000-000000000003" // no panel role
	guardStronger = "10000000-0000-4000-8000-000000000004" // users.write + panelroles.write
)

type guardFakeStore struct {
	store.Store
	users   map[string]*models.User
	roleOf  map[string]int
	roles   map[int]*store.PanelRole
	touched []string // what a handler changed, by user id
}

func (f *guardFakeStore) GetUserByID(id string) (*models.User, error) {
	if u, ok := f.users[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, errNotFoundForGuard
}

func (f *guardFakeStore) GetUserPanelAuthz(id string) (*int, store.CapOverrides, error) {
	if rid, ok := f.roleOf[id]; ok {
		return &rid, store.CapOverrides{}, nil
	}
	return nil, store.CapOverrides{}, nil
}

func (f *guardFakeStore) GetPanelRole(id int) (*store.PanelRole, error) { return f.roles[id], nil }

func (f *guardFakeStore) GetSetting(string) (string, error) { return "", nil }

func (f *guardFakeStore) UpdateUserPassword(id, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}

func (f *guardFakeStore) DisableUserTOTP(id string) error {
	f.touched = append(f.touched, id)
	return nil
}

func (f *guardFakeStore) SetUserRole(id, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}

func (f *guardFakeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

// The email route looks for a collision before it re-authenticates, so the
// fake has to answer. Nobody holds the address these cases try.
func (f *guardFakeStore) GetUserByEmail(string) (*models.User, error) { return nil, nil }

func (f *guardFakeStore) SetUserPermissionFlags(id string, _, _ bool, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}

func (f *guardFakeStore) UsernameTaken(string, string) (bool, error) { return false, nil }

func (f *guardFakeStore) RenameUser(id, _, _ string) error {
	f.touched = append(f.touched, id)
	return nil
}

func newGuardFakeStore() *guardFakeStore {
	return &guardFakeStore{
		users: map[string]*models.User{
			guardStaff:    {ID: guardStaff, Username: "staff", Role: "user"},
			guardAdmin:    {ID: guardAdmin, Username: "admin", Role: "admin", IsAdmin: true},
			guardMember:   {ID: guardMember, Username: "member", Role: "user"},
			guardStronger: {ID: guardStronger, Username: "stronger", Role: "user"},
		},
		roleOf: map[string]int{guardStaff: 1, guardStronger: 2},
		roles: map[int]*store.PanelRole{
			1: {ID: 1, Capabilities: []string{"users.read", "users.write", "users.delete"}},
			2: {ID: 2, Capabilities: []string{"users.read", "users.write", "users.delete", "panelroles.write"}},
		},
	}
}

func guardState(fs *guardFakeStore) *handlers.AppState {
	return &handlers.AppState{Store: fs, Authz: authz.NewResolver(fs)}
}

func guardRequest(method, actor string, isAdmin bool, target, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "userID", actor)
	ctx = context.WithValue(ctx, "username", "actor")
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	return mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": target})
}

type keyedGuardStore struct {
	*guardFakeStore
	keys    []models.APIKey
	revoked []int
}

func (f *keyedGuardStore) ListAPIKeysByUser(string) ([]models.APIKey, error) { return f.keys, nil }

func (f *keyedGuardStore) RevokeAPIKey(id int, _ string) error {
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *keyedGuardStore) DeleteAllSSHKeys(string) (int, error) { return 0, nil }

type faultyRoleStore struct {
	*guardFakeStore
	failFor string
}

func (f *faultyRoleStore) GetUserPanelAuthz(id string) (*int, store.CapOverrides, error) {
	if id == f.failFor {
		return nil, store.CapOverrides{}, guardErr("connection reset by peer")
	}
	return f.guardFakeStore.GetUserPanelAuthz(id)
}

func emailStore() *emailFakeStore {
	u := &models.User{ID: emailTestUserID, Username: "cust", Email: "old@example.com"}
	return &emailFakeStore{
		users: map[string]*models.User{
			emailTestUserID: u,
			// The administrator doing the asking, with a password the
			// re-authentication can actually verify.
			"admin-1": {ID: "admin-1", Username: "admin", Password: testReauthHash},
		},
		byEmail:  map[string]*models.User{"old@example.com": u},
		settings: map[string]string{},
	}
}

type emailFakeStore struct {
	store.Store
	users    map[string]*models.User
	byEmail  map[string]*models.User
	settings map[string]string

	setEmailCalls []struct{ id, email string }
	tokenSetFor   string
	pending       map[string]string
	byToken       *models.User
	confirmed     []string
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

func (f *emailFakeStore) SetPendingEmail(id, email, _ string) error {
	if f.pending == nil {
		f.pending = map[string]string{}
	}
	if email == "" {
		delete(f.pending, id)
		return nil
	}
	f.pending[id] = email
	return nil
}

func (f *emailFakeStore) ConfirmPendingEmail(id string) (string, string, bool, error) {
	u, p := f.users[id], f.pending[id]
	if u == nil || p == "" {
		return "", "", false, nil
	}
	old := u.Email
	u.Email = p
	delete(f.pending, id)
	f.confirmed = append(f.confirmed, p)
	return old, p, true, nil
}

func (f *emailFakeStore) GetUserByEmailVerificationToken(string) (*models.User, error) {
	if f.byToken == nil {
		return nil, sql.ErrNoRows
	}
	c := *f.byToken
	return &c, nil
}

func (f *emailFakeStore) MarkEmailVerified(string) error { return nil }

func emailReq(t *testing.T, st *emailFakeStore, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	h := handlers.NewUserEmailHandler(&handlers.AppState{Store: st})
	r := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+emailTestUserID+"/email", bytes.NewReader([]byte(withReauthJSON(body))))
	r = mux.SetURLVars(r, map[string]string{"id": emailTestUserID})
	ctx := context.WithValue(r.Context(), "userID", "admin-1")
	r = r.WithContext(context.WithValue(ctx, "isAdmin", true))
	w := httptest.NewRecorder()
	h.SetEmail(w, r)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func rsaPub(t *testing.T, bits int) ssh.PublicKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// The admin account-management actions re-authenticate, so every test that
// drives one to its WRITE has to prove who is asking. One password and one
// hash for the whole package, at MinCost because these run on every build.
//
// A fixture that wants to exercise the refusal simply leaves the block out.
const testReauthPassword = "correct-horse-battery-staple"

var testReauthHash = mustHash(testReauthPassword)

func mustHash(pw string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}

// withReauth adds the acting administrator's credential to a request body.
func withReauth(body map[string]interface{}) map[string]interface{} {
	if body == nil {
		body = map[string]interface{}{}
	}
	body["reauth"] = map[string]string{"password": testReauthPassword}
	return body
}

type sentMail struct{ key, to string }

func nextMail(t *testing.T, ch <-chan sentMail) sentMail {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no notice was sent")
	}
	return sentMail{}
}

// captureAccountMail records the account notices; a copy of
// handlers/account_change_notice_test.go's, swapping handlers' mail sender
// through SetAccountMailSender because the notices go out from there.
func captureAccountMail(t *testing.T) <-chan sentMail {
	t.Helper()
	ch := make(chan sentMail, 8)
	t.Cleanup(handlers.SetAccountMailSender(func(_ services.MailStore, key, to, _ string, _ map[string]string) error {
		ch <- sentMail{key, to}
		return nil
	}))
	return ch
}

type guardErr string

func (e guardErr) Error() string { return string(e) }

const errNotFoundForGuard = guardErr("not found")

const emailTestUserID = "11111111-1111-1111-1111-111111111111"

// withReauthJSON adds the acting administrator's credential to a body these
// cases write as raw JSON. Changing an address is a password change by other
// means and re-authenticates now; a body that is not valid JSON is passed
// through untouched, since the cases that send one are about the decode.
func withReauthJSON(body string) string {
	var m map[string]interface{}
	if json.Unmarshal([]byte(body), &m) != nil {
		return body
	}
	m["reauth"] = map[string]string{"password": testReauthPassword}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(out)
}
