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

type deleteUserFakeStore struct {
	store.Store
	deleteErr error
}

func (f *deleteUserFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "customer"}, nil
}

func (f *deleteUserFakeStore) DeleteUser(string) error { return f.deleteErr }

// The account teardown now asks these before it destroys anything, so the fake
// has to answer them. Zero and empty: this test is about the message DeleteUser
// produces, not about what the account holds.
func (f *deleteUserFakeStore) CountServersByOwner(string) (int, error) { return 0, nil }
func (f *deleteUserFakeStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return nil, nil
}
func (f *deleteUserFakeStore) ListAllWarpAPIKeysByOwner(o string) ([]store.WarpAPIKey, error) {
	return f.ListWarpAPIKeysByOwner(o)
}
func (f *deleteUserFakeStore) ListNodesByOwner(string) ([]models.Node, error)     { return nil, nil }
func (f *deleteUserFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) { return nil, nil }

func deleteUserRequest() *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/users/11111111-1111-1111-1111-111111111111", nil)
	ctx := context.WithValue(req.Context(), "username", "admin")
	ctx = context.WithValue(ctx, "userID", "admin-id")
	ctx = context.WithValue(ctx, "isAdmin", true)
	return mux.SetURLVars(req.WithContext(ctx),
		map[string]string{"id": "11111111-1111-1111-1111-111111111111"})
}

// servers.owner_id is REFERENCES users(id) with no ON DELETE clause, so Postgres
// refuses to delete a user who still owns servers. That refusal is correct - the
// alternative is servers with no owner - and Postgres says exactly why:
//
//	violates foreign key constraint "servers_owner_id_fkey" on table "servers"
//
// The handler collapsed all of that into a bare "Delete failed" with a 500. An
// admin then has a button that does nothing, a status code that blames the
// server for what is actually the state of the data, and no hint that the remedy
// is to transfer or delete those servers first.
func TestDeleteUserReportsWhyItCannotDeleteAnOwner(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "still owns servers",
			err:         store.ErrUserOwnsServers,
			wantStatus:  http.StatusConflict,
			wantMessage: "owns servers",
		},
		{
			// server_invites.invited_by is the other NO ACTION reference: a user
			// who invited members cannot go while those invites exist, even
			// owning nothing themselves.
			name:        "still referenced elsewhere",
			err:         store.ErrUserStillReferenced,
			wantStatus:  http.StatusConflict,
			wantMessage: "still referenced",
		},
		{
			// A genuine fault stays a 500. The point is to separate "cannot yet"
			// from "went wrong", not to turn every failure into a 409.
			name:        "a real fault is still a 500",
			err:         errors.New("connection refused"),
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "Delete failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &deleteUserFakeStore{deleteErr: c.err}
			h := &UserHandler{state: &AppState{Store: fs}}
			rw := httptest.NewRecorder()

			h.DeleteUser(rw, deleteUserRequest())

			if rw.Code != c.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rw.Code, c.wantStatus, rw.Body.String())
			}
			if !strings.Contains(rw.Body.String(), c.wantMessage) {
				t.Errorf("body = %s, want it to mention %q", rw.Body.String(), c.wantMessage)
			}
		})
	}
}

// The ordinary case must not have picked up a conflict branch by accident.
func TestDeleteUserSucceeds(t *testing.T) {
	fs := &deleteUserFakeStore{}
	h := &UserHandler{state: &AppState{Store: fs}}
	rw := httptest.NewRecorder()

	h.DeleteUser(rw, deleteUserRequest())

	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rw.Code, rw.Body.String())
	}
}
