package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// The gate on SetMyQuestions, asserted where it lives.
//
// These answers ARE the password-reset path. A session that can rewrite them
// owns the account's recovery from then on - it survives the owner changing
// their password and even revoking every API key, so it is the more permanent
// of the two doors requireReauth closes.
type secQFakeStore struct {
	store.Store
	users    map[string]*models.User
	settings map[string]string
	written  int
}

func (f *secQFakeStore) GetSetting(key string) (string, error) { return f.settings[key], nil }

func (f *secQFakeStore) GetUserByID(id string) (*models.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, errors.New("user not found")
	}
	return u, nil
}

func (f *secQFakeStore) SetUserSecurityQuestions(userID, hashedJSON string) error {
	f.written++
	return nil
}

// The success path audits the change, so the fake has to answer that call.
func (f *secQFakeStore) InsertAuditIdentity(e *models.AuditEventIdentity) error { return nil }

func secQRequest(userID string, body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("PUT", "/api/me/security-questions", bytes.NewReader(b))
	return r.WithContext(context.WithValue(r.Context(), "userID", userID))
}

func secQHandler(fs *secQFakeStore) *SecurityQuestionsHandler {
	if fs.settings == nil {
		fs.settings = map[string]string{}
	}
	fs.settings["auth.security_questions_enabled"] = "true"
	fs.settings["auth.security_questions_count"] = "1"
	fs.settings["auth.security_questions_pool"] = `["What was your first pet?"]`
	return NewSecurityQuestionsHandler(Deps{Store: fs})
}

func TestSetMyQuestionsRefusesTheWrongAccountPassword(t *testing.T) {
	for name, password := range map[string]string{
		"wrong":   "not-the-password",
		"missing": "",
	} {
		t.Run(name+" password", func(t *testing.T) {
			fs := &secQFakeStore{users: map[string]*models.User{
				"u1": {ID: "u1", Password: reauthTestHash},
			}}
			h := secQHandler(fs)
			rec := httptest.NewRecorder()

			h.SetMyQuestions(rec, secQRequest("u1", map[string]interface{}{
				"items":    []map[string]string{{"question": "What was your first pet?", "answer": "rex"}},
				"password": password,
			}))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if fs.written != 0 {
				t.Errorf("the recovery answers were rewritten without the account password")
			}
		})
	}
}

func TestSetMyQuestionsAcceptsTheAccountPassword(t *testing.T) {
	fs := &secQFakeStore{users: map[string]*models.User{
		"u1": {ID: "u1", Password: reauthTestHash},
	}}
	h := secQHandler(fs)
	rec := httptest.NewRecorder()

	h.SetMyQuestions(rec, secQRequest("u1", map[string]interface{}{
		"items":    []map[string]string{{"question": "What was your first pet?", "answer": "rex"}},
		"password": reauthTestPassword,
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.written != 1 {
		t.Errorf("writes = %d, want 1 - the gate refused a correct password", fs.written)
	}
}
