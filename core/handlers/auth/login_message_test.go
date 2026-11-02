package auth

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"golang.org/x/crypto/bcrypt"
)

// loginFakeStore serves one known user to LoginHandler.
type loginFakeStore struct {
	store.Store
	user *models.User
	err  error
}

func (f *loginFakeStore) GetUserByUsername(string) (*models.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}
func (f *loginFakeStore) GetSetting(string) (string, error) { return "", nil }

// TestLogin_RejectionIsIndistinguishable: an unknown username and a wrong
// password must produce the SAME status and the SAME message. A distinct
// "User not found" turns the login form into a username oracle: an attacker
// learns which accounts exist before ever guessing a password.
//
// The panel-side complaint that started this ("Request failed (401)") had a
// different cause - a double body read, covered by panel/src/lib/api/login.test.ts
// - but it is the reason the wording was looked at at all.
func TestLogin_RejectionIsIndistinguishable(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	known := &models.User{ID: "user-1", Username: "alice", Password: string(hash)}

	tests := []struct {
		name  string
		fake  *loginFakeStore
		body  string
		field string
	}{
		{
			name:  "unknown username",
			fake:  &loginFakeStore{err: sql.ErrNoRows},
			body:  `{"username":"nobody","password":"whatever"}`,
			field: "no such user",
		},
		{
			name:  "known username, wrong password",
			fake:  &loginFakeStore{user: known},
			body:  `{"username":"alice","password":"wrong"}`,
			field: "bad password",
		},
	}

	var seen []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &AuthHandler{
				d:      Deps{StoreEnabled: true, Store: tt.fake},
				jwtKey: []byte("test-jwt-secret-value-not-a-real-secret"),
			}
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			h.LoginHandler(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401", tt.field, rec.Code)
			}
			var got struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("%s: body is not JSON (%v): %s", tt.field, err, rec.Body.String())
			}
			// The panel quotes this message verbatim, so an empty one would put
			// the user back at a bare status code.
			if got.Message == "" {
				t.Fatalf("%s: empty message", tt.field)
			}
			seen = append(seen, got.Message)
		})
	}

	if len(seen) == 2 && seen[0] != seen[1] {
		t.Errorf("wrong-user and wrong-password differ: %q vs %q — that is a username oracle", seen[0], seen[1])
	}
}
