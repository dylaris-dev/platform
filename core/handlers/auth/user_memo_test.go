package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// countingUserStore counts user-row reads.
type countingUserStore struct {
	store.Store
	user  *models.User
	reads int
}

func (f *countingUserStore) GetUserByUsername(string) (*models.User, error) {
	f.reads++
	cp := *f.user
	return &cp, nil
}
func (f *countingUserStore) GetSetting(string) (string, error) { return "", nil }

// GET /api/auth/profile reads the user row once per request: the profile
// handler reuses the row AuthMiddleware loaded, and clearing the password for
// the response does not touch the row the middleware handed on.
func TestProfileReusesTheMiddlewareUserRow(t *testing.T) {
	const secret = "test-jwt-secret-value-not-a-real-secret"
	fake := &countingUserStore{user: &models.User{ID: "u1", Username: "alice", Password: "hash"}}
	h := &AuthHandler{d: Deps{Store: fake}, jwtKey: []byte(secret)}
	token, err := h.IssueToken("alice", false, "hash")
	if err != nil {
		t.Fatal(err)
	}
	var after *models.User
	handler := h.AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		h.GetProfileHandler(w, r)
		after = authz.UserFromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if fake.reads != 1 {
		t.Fatalf("user row read %d times, want 1", fake.reads)
	}
	var body struct {
		User models.User `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.User.Username != "alice" || body.User.Password != "" {
		t.Fatalf("profile body %s (%v)", rec.Body.String(), err)
	}
	if after == nil || after.Password != "hash" {
		t.Fatalf("the handler's edit reached the request's memo: %+v", after)
	}
}

// A request that did not come through the middleware (no memo) still reads
// the row itself, and a memo for a different name is not used.
func TestProfileWithoutMemoReadsTheStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		memo *models.User
	}{
		{"no memo", nil},
		{"memo of another user", &models.User{ID: "u2", Username: "bob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingUserStore{user: &models.User{ID: "u1", Username: "alice"}}
			h := &AuthHandler{d: Deps{Store: fake}}
			req := httptest.NewRequest(http.MethodGet, "/api/auth/profile", nil)
			ctx := authz.WithUser(req.Context(), tc.memo)
			ctx = context.WithValue(ctx, "username", "alice")
			rec := httptest.NewRecorder()
			h.GetProfileHandler(rec, req.WithContext(ctx))
			if fake.reads != 1 || rec.Code != http.StatusOK {
				t.Fatalf("reads %d status %d", fake.reads, rec.Code)
			}
		})
	}
}
