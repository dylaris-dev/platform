package reauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"dylaris-core/models"
	"dylaris-core/store"
)

type userStore struct {
	store.Store
	user *models.User
}

func (s userStore) GetUserByID(string) (*models.User, error) {
	if s.user == nil {
		return nil, errors.New("not found")
	}
	return s.user, nil
}

// RequireAdminReauth took *AppState and called the root's code check before
// it moved here; it now takes the store and the check. These pin the answer
// for each way it can go, status and header, so a slip in the move (a lost
// 503, a 2FA account let through on the password, a limit answered as a wrong
// code) fails by name.
func TestTheAdminReauthAnswersAsBeforeTheMove(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), 4)
	if err != nil {
		t.Fatal(err)
	}
	plain := &models.User{ID: "a1", Password: string(hash)}
	twoFA := &models.User{ID: "a1", Password: string(hash), Is2FAEnabled: true}
	called := 0
	verify := func(ok bool, err error) CodeVerifier {
		return func(*models.User, string) (bool, error) { called++; return ok, err }
	}
	req := func(actor string) *http.Request {
		r := httptest.NewRequest(http.MethodPut, "/", nil)
		return r.WithContext(context.WithValue(r.Context(), "userID", actor))
	}
	body := func(pw string) AdminReauthRequest {
		return AdminReauthRequest{Reauth: ReauthFields{Password: pw, Code: "123456"}}
	}

	for _, c := range []struct {
		name   string
		actor  string
		st     store.Store
		verify CodeVerifier
		pw     string
		code   int // 0: allowed
		header bool
		calls  int
	}{
		{"no actor", "", userStore{user: plain}, verify(true, nil), "pw", http.StatusUnauthorized, false, 0},
		{"no store", "a1", nil, verify(true, nil), "pw", http.StatusServiceUnavailable, true, 0},
		{"unknown actor", "a1", userStore{}, verify(true, nil), "pw", http.StatusUnauthorized, true, 0},
		{"wrong password", "a1", userStore{user: plain}, verify(true, nil), "nope", http.StatusUnauthorized, true, 0},
		{"no 2FA: the password is the credential", "a1", userStore{user: plain}, verify(false, nil), "pw", 0, false, 0},
		{"2FA, right code", "a1", userStore{user: twoFA}, verify(true, nil), "pw", 0, false, 1},
		{"2FA, wrong code", "a1", userStore{user: twoFA}, verify(false, nil), "pw", http.StatusUnauthorized, true, 1},
		{"2FA, over the limit", "a1", userStore{user: twoFA}, verify(false, ErrTooManyCodes), "pw", http.StatusTooManyRequests, true, 1},
		{"2FA, check failed", "a1", userStore{user: twoFA}, verify(false, errors.New("db")), "pw", http.StatusInternalServerError, true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			called = 0
			rec := httptest.NewRecorder()
			ok := RequireAdminReauth(rec, req(c.actor), c.st, c.verify, body(c.pw))
			if ok != (c.code == 0) || (c.code != 0 && rec.Code != c.code) {
				t.Fatalf("ok=%v code=%d, want code %d", ok, rec.Code, c.code)
			}
			if got := rec.Header().Get(FailedHeader) == "failed"; got != c.header {
				t.Fatalf("reauth header set=%v, want %v", got, c.header)
			}
			if called != c.calls {
				t.Fatalf("code check ran %d times, want %d", called, c.calls)
			}
		})
	}
}
