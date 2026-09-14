package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

const targetID = "22222222-2222-2222-2222-222222222222"

type lastAdminFakeStore struct {
	store.Store
	target  models.User
	users   []models.User
	listErr error
	deleted bool
}

func (f *lastAdminFakeStore) GetUserByID(string) (*models.User, error) {
	u := f.target
	return &u, nil
}
func (f *lastAdminFakeStore) ListUsers() ([]models.User, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.users, nil
}
func (f *lastAdminFakeStore) DeleteUser(string) error { f.deleted = true; return nil }

func lastAdminRequest() *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/users/"+targetID, nil)
	ctx := context.WithValue(req.Context(), "username", "someone-else")
	ctx = context.WithValue(ctx, "userID", "actor-id")
	// An admin: the last-admin rule is the question here. A non-admin may not
	// delete an admin at all (account_guard_test.go).
	ctx = context.WithValue(ctx, "isAdmin", true)
	return mux.SetURLVars(req.WithContext(ctx), map[string]string{"id": targetID})
}

// Asked by the account teardown before it destroys anything; this test is
// about a different question, so the answers are empty.
func (f *lastAdminFakeStore) CountServersByOwner(string) (int, error)        { return 0, nil }
func (f *lastAdminFakeStore) ListNodesByOwner(string) ([]models.Node, error) { return nil, nil }
func (f *lastAdminFakeStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return nil, nil
}
func (f *lastAdminFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) { return nil, nil }

// "You cannot delete yourself" does NOT keep an admin in the system:
// users.delete is a delegatable capability, so a non-admin holding it could
// remove every admin and lock the owner out of their own panel with nothing
// short of database access to get back in.
func TestDeleteUserRefusesTheLastAdmin(t *testing.T) {
	admin := models.User{ID: targetID, Username: "owner", IsAdmin: true}
	member := models.User{ID: "33333333-3333-3333-3333-333333333333", Username: "member"}
	otherAdmin := models.User{ID: "44444444-4444-4444-4444-444444444444", Username: "second", IsAdmin: true}

	tests := []struct {
		name        string
		target      models.User
		users       []models.User
		listErr     error
		wantStatus  int
		wantMessage string
		wantDeleted bool
	}{
		{
			name:        "the only admin is refused",
			target:      admin,
			users:       []models.User{admin, member},
			wantStatus:  http.StatusConflict,
			wantMessage: "last admin",
		},
		{
			name:        "an admin goes once a second one exists",
			target:      admin,
			users:       []models.User{admin, otherAdmin, member},
			wantStatus:  http.StatusOK,
			wantDeleted: true,
		},
		{
			// The guard is about admins; an ordinary member is unaffected even
			// when they are the only user besides the admin.
			name:        "a member is unaffected",
			target:      member,
			users:       []models.User{admin, member},
			wantStatus:  http.StatusOK,
			wantDeleted: true,
		},
		{
			// Refuse when the count is UNKNOWN. Guessing "another admin exists"
			// is the one direction that cannot be undone from the panel.
			name:        "an unreadable user list refuses rather than assumes",
			target:      admin,
			users:       []models.User{admin, otherAdmin},
			listErr:     errors.New("connection reset"),
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: "how many admins",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &lastAdminFakeStore{target: tt.target, users: tt.users, listErr: tt.listErr}
			h := &UserHandler{state: &AppState{Store: fs}}
			rw := httptest.NewRecorder()

			h.DeleteUser(rw, lastAdminRequest())

			if rw.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rw.Code, tt.wantStatus, rw.Body.String())
			}
			if tt.wantMessage != "" && !strings.Contains(rw.Body.String(), tt.wantMessage) {
				t.Errorf("body = %s, want it to mention %q", rw.Body.String(), tt.wantMessage)
			}
			if fs.deleted != tt.wantDeleted {
				t.Errorf("deleted = %t, want %t", fs.deleted, tt.wantDeleted)
			}
		})
	}
}
