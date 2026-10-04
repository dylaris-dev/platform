package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"dylaris-core/authz"

	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/services"
)

type sentMail struct{ key, to string }

// captureAccountMail records the notices, which go out on their own goroutine.
func captureAccountMail(t *testing.T) <-chan sentMail {
	t.Helper()
	ch := make(chan sentMail, 8)
	orig := sendAccountMail
	sendAccountMail = func(_ services.MailStore, key, to, _ string, _ map[string]string) error {
		ch <- sentMail{key, to}
		return nil
	}
	t.Cleanup(func() { sendAccountMail = orig })
	return ch
}

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

// With 2FA on, the address and the password asked for the password alone: a
// taken session plus a phished password moved every future reset elsewhere
// and locked the owner out of an account protected by a second factor.
func TestChangingTheEmailOrPasswordNeedsTheSecondFactor(t *testing.T) {
	secret := freshTOTPSecret(t)
	for _, body := range []map[string]interface{}{
		{"email": "new@example.test"},
		{"newPassword": "a-brand-new-long-password"},
	} {
		st := newProfileStore(t, false)
		st.users["u-me"].Is2FAEnabled = true
		st.users["u-me"].TOTPSecret = secret

		w := saveProfile(t, st, copyBody(body))
		// 403, not 401: the panel reads a 401 as an expired session.
		if w.Code != http.StatusForbidden || len(st.setEmailCalls)+len(st.passwordWrites) != 0 {
			t.Fatalf("%v without a code: status %d, writes %d/%d", body, w.Code, len(st.setEmailCalls), len(st.passwordWrites))
		}
		withWrong := copyBody(body)
		withWrong["totpCode"] = "000000"
		if w := saveProfile(t, st, withWrong); w.Code != http.StatusForbidden || len(st.setEmailCalls)+len(st.passwordWrites) != 0 {
			t.Fatalf("%v with a wrong code: status %d", body, w.Code)
		}
		code, _ := totp.GenerateCode(secret, time.Now())
		withRight := copyBody(body)
		withRight["totpCode"] = code
		if w := saveProfile(t, st, withRight); w.Code != http.StatusOK || len(st.setEmailCalls)+len(st.passwordWrites) != 1 {
			t.Fatalf("%v with the right code: status %d: %s", body, w.Code, w.Body)
		}
	}
	// Without 2FA nothing changes: the password is the whole credential.
	st := newProfileStore(t, false)
	if w := saveProfile(t, st, map[string]interface{}{"email": "new@example.test"}); w.Code != http.StatusOK {
		t.Fatalf("an account without 2FA was asked for a code: %d", w.Code)
	}
}

func copyBody(b map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// The previous address hears about a change of address, and about a change of
// password - in one save that does both, the old address hears both, because
// the new one is the one that cannot be trusted to tell the owner.
func TestTheOldAddressHearsAboutTheChange(t *testing.T) {
	mails := captureAccountMail(t)
	st := newProfileStore(t, false)
	w := saveProfile(t, st, map[string]interface{}{"email": "new@example.test", "newPassword": "a-brand-new-long-password"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got := map[string]string{}
	for i := 0; i < 2; i++ {
		m := nextMail(t, mails)
		got[m.key] = m.to
	}
	if got[mailer.KeyEmailChanged] != "me@example.test" || got[mailer.KeyPasswordChanged] != "me@example.test" {
		t.Fatalf("notices %v, want both to the previous address", got)
	}
}

// An operator account changing addresses or setting passwords is also what a
// compromised operator account looks like; the owner hears about both.
func TestTheOwnerHearsAboutAnOperatorsChange(t *testing.T) {
	t.Run("address", func(t *testing.T) {
		mails := captureAccountMail(t)
		st := emailStore()
		if w, _ := emailReq(t, st, `{"email":"new@example.com"}`); w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		if m := nextMail(t, mails); m.key != mailer.KeyEmailChanged || m.to != "old@example.com" {
			t.Fatalf("notice %+v, want the address-change notice to the old address", m)
		}
	})
	t.Run("password", func(t *testing.T) {
		mails := captureAccountMail(t)
		base := newGuardFakeStore()
		base.users[guardAdmin].Password = testReauthHash
		base.users[guardMember].Email = "member@example.test"
		fs := &keyedGuardStore{guardFakeStore: base}
		h := NewUserHandler(&AppState{Store: fs, Authz: authz.NewResolver(fs)})
		rec := httptest.NewRecorder()
		h.ResetUserPassword(rec, guardRequest("PUT", guardAdmin, true, guardMember,
			`{"password":"a-brand-new-password","reauth":{"password":"`+testReauthPassword+`"}}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		if m := nextMail(t, mails); m.key != mailer.KeyPasswordChanged || m.to != "member@example.test" {
			t.Fatalf("notice %+v", m)
		}
	})
}

// The code step on the profile shares the login's per-account limit: a session
// holder guessing codes there is the same attack.
func TestTheProfileCodeStepIsLimitedPerAccount(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	st := newProfileStore(t, false)
	st.users["u-me"].Is2FAEnabled = true
	st.users["u-me"].TOTPSecret = freshTOTPSecret(t)
	h := NewAuthHandler(&AppState{Store: st, Redis: rdb}, "test-secret")
	save := func() int {
		raw, _ := json.Marshal(map[string]interface{}{"oldPassword": profilePassword, "email": "new@example.test", "totpCode": "000000"})
		r := httptest.NewRequest(http.MethodPut, "/api/auth/profile", bytes.NewReader(raw))
		r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "username", "me"), "isAdmin", false))
		w := httptest.NewRecorder()
		h.UpdateProfileHandler(w, r)
		return w.Code
	}
	for i := 0; i < wrongCodeLimit; i++ {
		save()
	}
	if code := save(); code != http.StatusTooManyRequests {
		t.Fatalf("past the limit: %d, want 429", code)
	}
}

// The limit sits in the one code check every caller uses. On the login alone
// it left 2FA disable open to unlimited guessing from many addresses - and
// with 2FA off the address changes without a code.
func TestTheCodeLimitCoversTwoFactorDisable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	fs := &sessionsFakeStore{user: &models.User{ID: "u1", Username: "alice", Password: string(hash), Is2FAEnabled: true, TOTPSecret: freshTOTPSecret(t)}}
	h := &AuthHandler{state: &AppState{Store: fs, Redis: rdb}, jwtKey: []byte(testJWTSecret)}
	for i := 0; i < wrongCodeLimit; i++ {
		if rr := postJSONAs(t, h.DisableTOTPHandler, "alice", DisableTOTPRequest{Password: "pw", Code: "000000"}); rr.Code != http.StatusForbidden {
			t.Fatalf("wrong code %d: %d", i+1, rr.Code)
		}
	}
	if rr := postJSONAs(t, h.DisableTOTPHandler, "alice", DisableTOTPRequest{Password: "pw", Code: "000000"}); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("past the limit: %d, want 429", rr.Code)
	}
}

// codeSpyStore notices a backup code being spent.
type codeSpyStore struct {
	*profileFakeStore
	spent int
}

func (f *codeSpyStore) ConsumeTOTPBackupCode(string, string, string) (bool, error) {
	f.spent++
	return true, nil
}

// A save that is refused for something else must not spend the code: a
// backup code is gone for good once spent.
func TestARefusedSaveDoesNotSpendTheCode(t *testing.T) {
	base := newProfileStore(t, false)
	base.users["u-me"].Is2FAEnabled = true
	base.users["u-me"].TOTPBackupCodes = bcryptBackups(t, []string{"abcdef0123456789"})
	st := &codeSpyStore{profileFakeStore: base}
	raw, _ := json.Marshal(map[string]interface{}{
		"oldPassword": profilePassword, "email": "new@example.test",
		"minecraftUsername": "not a valid name!", "totpCode": "abcdef0123456789",
	})
	h := NewAuthHandler(&AppState{Store: st}, "test-secret")
	r := httptest.NewRequest(http.MethodPut, "/api/auth/profile", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "username", "me"), "isAdmin", false))
	w := httptest.NewRecorder()
	h.UpdateProfileHandler(w, r)
	if w.Code != http.StatusBadRequest || st.spent != 0 {
		t.Fatalf("status %d, codes spent %d: a refused save spent the code", w.Code, st.spent)
	}
}
