package handlers

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

type forgotAsyncStore struct {
	store.Store
	settings map[string]string
}

func (f *forgotAsyncStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *forgotAsyncStore) GetUserByEmail(string) (*models.User, error) {
	return &models.User{ID: "u1", Username: "alice", Email: "alice@example.test"}, nil
}
func (f *forgotAsyncStore) SetPasswordResetToken(string, string, time.Time, time.Time) (bool, error) {
	return true, nil
}
func (f *forgotAsyncStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }
func (f *forgotAsyncStore) GetMailTemplate(string) (*models.MailTemplate, error) { return nil, nil }

// A known address answered after a full mail round trip and an unknown one at
// once, so the time told which addresses are on file - and a relay that hung
// hung the request with it. The mail now goes out in the background: against
// a relay that never answers, the request still returns at once.
func TestForgotPasswordDoesNotWaitForTheMail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) // accept, never greet
		}
	}()
	fs := &forgotAsyncStore{settings: map[string]string{
		"smtp.default.host":       "127.0.0.1",
		"smtp.default.port":       strconv.Itoa(ln.Addr().(*net.TCPAddr).Port),
		"smtp.default.encryption": "none",
		"smtp.default.from_email": "noreply@example.test",
	}}
	h := NewPasswordResetHandler(&AppState{Store: fs})

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ForgotPassword(rec, httptest.NewRequest("POST", "/api/auth/forgot-password", strings.NewReader(`{"email":"alice@example.test"}`)))
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the request waited %s for a relay that never answers", took)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
