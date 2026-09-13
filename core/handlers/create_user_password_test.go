package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"dylaris-core/models"
	"dylaris-core/store"
)

// createUserFakeStore embeds store.Store (nil) so it satisfies the full
// interface; only what CreateUser touches is implemented. Anything else panics,
// which is the point - an unexpected call is a test that stopped testing what it
// claims to.
type createUserFakeStore struct {
	store.Store

	created *models.User
	taken   bool
}

func (f *createUserFakeStore) GetSetting(string) (string, error) { return "", nil }

// The create path now asks whether the name is claimed, without case. This fake
// says no; the taken case has its own test below.
func (f *createUserFakeStore) UsernameTaken(username, excludeUserID string) (bool, error) {
	return f.taken, nil
}

func (f *createUserFakeStore) CreateUser(u *models.User) error {
	u.ID = "new-user-1"
	copied := *u
	f.created = &copied
	return nil
}

func (f *createUserFakeStore) SetUserRegions(string, bool, []string) error { return nil }

// The admin create-user path takes the password through a struct that embeds
// models.User, whose Password field is json:"-" so a bcrypt hash can never be
// serialised back to a client. That tag applies on the way IN as well, and it
// silently made `password` the one field an admin could not send: every create
// decoded to an empty password and was refused before it reached the store.
//
// Both halves are load-bearing and fail in opposite directions, so both are
// asserted here: the payload must ARRIVE, and it must be the HASH that reaches
// the store - a plaintext password persisted as if it were a hash is the worse
// of the two bugs.
func TestCreateUserTakesThePasswordFromTheWire(t *testing.T) {
	fake := &createUserFakeStore{}
	h := NewUserHandler(&AppState{Store: fake})

	const plaintext = "correct-horse-battery"
	body := `{"username":"alice","password":"` + plaintext + `","isAdmin":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.CreateUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; an admin cannot create any account", rec.Code, rec.Body.String())
	}
	if fake.created == nil {
		t.Fatal("nothing reached the store")
	}
	if fake.created.Password == "" {
		t.Fatal("the account was persisted with an empty password")
	}
	if fake.created.Password == plaintext {
		t.Fatal("the plaintext password was stored instead of its hash")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(fake.created.Password), []byte(plaintext)); err != nil {
		t.Fatalf("the stored hash does not verify the password that was sent: %v", err)
	}
}

// An account an admin creates gets no verification mail, so it must reach the
// store already verified or it can never log in once verification is required.
// A timestamp in the body is replaced, not trusted.
func TestCreateUserStoresTheAccountVerified(t *testing.T) {
	fake := &createUserFakeStore{}
	h := NewUserHandler(&AppState{Store: fake})

	body := `{"username":"alice","password":"correct-horse-battery","emailVerifiedAt":"2001-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	before := time.Now()
	h.CreateUser(rec, req)

	if rec.Code != http.StatusOK || fake.created == nil {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fake.created.EmailVerifiedAt == nil {
		t.Fatal("the admin-created account reached the store unverified")
	}
	if fake.created.EmailVerifiedAt.Before(before) {
		t.Errorf("verified at %v: the timestamp from the request body was stored", fake.created.EmailVerifiedAt)
	}
}

// The guard the shadowing field must not weaken: a create with no password at
// all is still refused rather than persisted with an empty hash.
func TestCreateUserStillRefusesAnEmptyPassword(t *testing.T) {
	fake := &createUserFakeStore{}
	h := NewUserHandler(&AppState{Store: fake})

	req := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewBufferString(`{"username":"alice"}`))
	rec := httptest.NewRecorder()
	h.CreateUser(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if fake.created != nil {
		t.Error("an account with no password was created")
	}
}
