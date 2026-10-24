package users

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/mailer"
)

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
		h := NewUserHandler(&handlers.AppState{Store: fs, Authz: authz.NewResolver(fs)})
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
