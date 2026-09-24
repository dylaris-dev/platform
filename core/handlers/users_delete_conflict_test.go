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
	deleteErr    error
	ownedServers int
	auditRows    []models.AuditEventIdentity
}

func (f *deleteUserFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "customer", Email: "customer@example.test"}, nil
}

func (f *deleteUserFakeStore) DeleteUser(string) error { return f.deleteErr }

// The account teardown now asks these before it destroys anything, so the fake
// has to answer them. Zero and empty: this test is about the message DeleteUser
// produces, not about what the account holds.
func (f *deleteUserFakeStore) CountServersByOwner(string) (int, error) {
	return f.ownedServers, nil
}
func (f *deleteUserFakeStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return nil, nil
}
func (f *deleteUserFakeStore) ListAllWarpAPIKeysByOwner(o string) ([]store.WarpAPIKey, error) {
	return f.ListWarpAPIKeysByOwner(o)
}
func (f *deleteUserFakeStore) ListNodesByOwner(string) ([]models.Node, error)     { return nil, nil }
func (f *deleteUserFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) { return nil, nil }

// Removing an account is on the record now, and the row is what the test
// below reads.
func (f *deleteUserFakeStore) InsertAuditIdentity(e *models.AuditEventIdentity) error {
	f.auditRows = append(f.auditRows, *e)
	return nil
}

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

// The same promise, one layer earlier. The teardown now asks "does this account
// still own servers" BEFORE anything is destroyed - which is right - but its
// refusal came back as a 500 with a message that named nothing, so the fix
// above was undone for the ordinary case. Measured on production: deleting an
// account that still owned one server answered 500 "Could not remove what this
// account still holds. Nothing was deleted.", and the sentence with the count
// went to the Core log, where the person clicking delete cannot read it.
func TestDeleteUserStillOwningServersIs409WithTheCount(t *testing.T) {
	fs := &deleteUserFakeStore{ownedServers: 2}
	h := &UserHandler{state: &AppState{Store: fs}}

	rr := httptest.NewRecorder()
	h.DeleteUser(rr, deleteUserRequest())

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %q)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"2", "Nothing was deleted"} {
		if !strings.Contains(body, want) {
			t.Errorf("message %q does not mention %q", body, want)
		}
	}
}

// Removing an account leaves no other trace, so the audit row has to carry
// enough to say what was removed.
//
// This path wrote nothing at all, while the auto-delete sweep beside it always
// did - so the removals an OPERATOR performs, which is most of them, were the
// ones missing from the record. Found the hard way: an account deleted here by
// mistake could only be reconstructed from its own registration row.
//
// The username and address are in the metadata because target_user_id is a
// foreign key onto users: deleting the user SETS IT NULL, and the row is left
// saying "some account was removed at this time".
func TestDeleteUserIsOnTheRecord(t *testing.T) {
	fs := &deleteUserFakeStore{}
	h := &UserHandler{state: &AppState{Store: fs}}

	rr := httptest.NewRecorder()
	h.DeleteUser(rr, deleteUserRequest())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}

	var row *models.AuditEventIdentity
	for i := range fs.auditRows {
		if fs.auditRows[i].EventType == AuditEventUserHardDeleted {
			row = &fs.auditRows[i]
		}
	}
	if row == nil {
		t.Fatalf("no %q row was written; the deletion left no trace at all", AuditEventUserHardDeleted)
	}
	if row.ActorUserID == nil || *row.ActorUserID == "" {
		t.Error("the row does not say WHO removed the account")
	}
	// No target id, deliberately: the column is a foreign key onto users and
	// the account is gone, so a row that names it is refused by the database.
	// The unit fake has no foreign key, which is why this assertion exists here
	// and a real-Postgres test exists next to it.
	if row.TargetUserID != nil {
		t.Errorf("the row points at %v, which no longer exists; Postgres refuses the insert", *row.TargetUserID)
	}
	if got := row.Metadata["userId"]; got == nil || got == "" {
		t.Error("the row does not carry the id of the account it was about")
	}
	if got := row.Metadata["username"]; got != "customer" {
		t.Errorf("metadata username = %v, want the account that was removed", got)
	}
	if got := row.Metadata["email"]; got != "customer@example.test" {
		t.Errorf("metadata email = %v; without it the row cannot identify the account once the row is gone", got)
	}
}

// A refused delete must not claim one happened.
func TestARefusedDeleteWritesNoRow(t *testing.T) {
	fs := &deleteUserFakeStore{ownedServers: 1}
	h := &UserHandler{state: &AppState{Store: fs}}

	rr := httptest.NewRecorder()
	h.DeleteUser(rr, deleteUserRequest())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	for _, row := range fs.auditRows {
		if row.EventType == AuditEventUserHardDeleted {
			t.Fatal("a refused delete was recorded as a deletion")
		}
	}
}
