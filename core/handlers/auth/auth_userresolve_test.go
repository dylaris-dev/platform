package auth

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// authResolveFakeStore controls what AuthMiddleware's user lookup returns.
// GetSetting is needed too: on the success path the demo gate consults it.
type authResolveFakeStore struct {
	store.Store
	user *models.User
	err  error
}

func (f *authResolveFakeStore) GetUserByUsername(string) (*models.User, error) {
	return f.user, f.err
}
func (f *authResolveFakeStore) GetSetting(string) (string, error) { return "", nil }

// TestAuthMiddleware_UserResolutionFailsClosed pins the fix for a lookup whose
// error used to be swallowed.
//
// The resolve block sat inside `if err == nil`, so a failed lookup produced a
// session that was authenticated but carried NO userID and had skipped the demo
// read-only gate. Two things followed: handlers that scope by owner assert
// userID and panicked on the missing value, and every mutating request from the
// demo account went through unchecked.
//
// The two failure modes must stay distinguishable. A deleted account is the
// caller's problem (401). A database fault is not, and answering 401 there would
// log everyone out during a blip and disguise the outage as bad credentials.
func TestAuthMiddleware_UserResolutionFailsClosed(t *testing.T) {
	const secret = "test-jwt-secret-value-not-a-real-secret"

	tests := []struct {
		name       string
		fake       *authResolveFakeStore
		wantStatus int
		wantNext   bool
	}{
		{
			name:       "row is gone - the token names someone who no longer exists",
			fake:       &authResolveFakeStore{err: sql.ErrNoRows},
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Some store implementations signal "not found" this way; the SSE
			// branch of the middleware already guards for it.
			name:       "nil user with no error",
			fake:       &authResolveFakeStore{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "database fault is not an authentication failure",
			fake:       &authResolveFakeStore{err: errors.New("connection reset by peer")},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "resolved - the request proceeds",
			fake:       &authResolveFakeStore{user: &models.User{ID: "user-1", Username: "alice"}},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &AuthHandler{
				d:      Deps{StoreEnabled: true, Store: tt.fake},
				jwtKey: []byte(secret),
			}
			token, err := h.IssueToken("alice", false, "")
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}

			var gotUserID any
			called := false
			next := func(w http.ResponseWriter, r *http.Request) {
				called = true
				gotUserID = r.Context().Value("userID")
			}

			req := httptest.NewRequest(http.MethodGet, "/api/gateway/link-routes", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			h.AuthMiddleware(next)(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if called != tt.wantNext {
				t.Fatalf("next called = %v, want %v", called, tt.wantNext)
			}
			if !tt.wantNext {
				return
			}
			// The point of failing closed: when the handler DOES run, userID is
			// present. Handlers scope ownership on it (rt.OwnerID == userID), so
			// an absent or empty value is not a smaller problem than a panic.
			id, ok := gotUserID.(string)
			if !ok || id == "" {
				t.Fatalf("userID in context = %#v, want a non-empty string", gotUserID)
			}
		})
	}
}
