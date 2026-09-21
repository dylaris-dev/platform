package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

// patchPermsStore records the write, because the point of these cases is that
// a body the endpoint did not understand must never reach it.
type patchPermsStore struct {
	store.Store
	updated bool
	written map[string]bool
}

func (f *patchPermsStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, OwnerID: "owner-1", OwnerName: "owner"}, nil
}
func (f *patchPermsStore) UpdateInvitePermissions(serverID int, userID string, perms map[string]bool) error {
	f.updated = true
	f.written = perms
	return nil
}
func (f *patchPermsStore) GetServerAuditState(int) (bool, bool, int, error) {
	return true, false, 0, nil
}
func (f *patchPermsStore) InsertServerAudit(*models.ServerAuditEvent) error { return nil }

func patchReq(body string) *http.Request {
	const target = "b0a6f0e0-0000-4000-8000-00000000abcd"
	r := httptest.NewRequest(http.MethodPatch, "/api/servers/1/members/"+target, bytes.NewReader([]byte(body)))
	r = mux.SetURLVars(r, map[string]string{"id": "1", "userId": target})
	ctx := context.WithValue(r.Context(), "userID", "owner-1")
	ctx = context.WithValue(ctx, "username", "owner")
	ctx = context.WithValue(ctx, "isAdmin", false)
	return r.WithContext(ctx)
}

// The invite was hardened after a live finding; the PATCH beside it kept
// accepting both mistakes. Measured on production against a member who could
// read the console: PATCH {"perms": {"console": true}} - one typo'd field name
// - answered 200 and stripped every permission they had, and PATCH with
// {"sudo": true} was accepted where the POST refuses it by name.
//
// Two endpoints, one decision, one guard: refuseBadPermissionMap.
func TestUpdateMemberPermissionsRefusesWhatTheInviteRefuses(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantWrite  bool
	}{
		{
			name:       "a field this endpoint does not know",
			body:       `{"perms":{"console":true}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a body with no permissions at all",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a permission key that means nothing",
			body:       `{"permissions":{"console":true,"sudo":true}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			// Taking everything away has to stay possible, or the refusals
			// above just move the problem.
			name:       "an empty set is a valid answer",
			body:       `{"permissions":{}}`,
			wantStatus: http.StatusOK,
			wantWrite:  true,
		},
		{
			name:       "an ordinary edit still goes through",
			body:       `{"permissions":{"console":true,"files":false}}`,
			wantStatus: http.StatusOK,
			wantWrite:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &patchPermsStore{}
			h := NewMemberHandler(&AppState{Store: fs})
			rec := httptest.NewRecorder()

			h.UpdateMemberPermissions(rec, patchReq(c.body))

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if fs.updated != c.wantWrite {
				t.Fatalf("permissions written = %v, want %v (%v)", fs.updated, c.wantWrite, fs.written)
			}
			if c.wantStatus == http.StatusOK {
				return
			}
			var out struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if out.Message == "" {
				t.Error("a refusal with no message leaves the caller guessing which field was wrong")
			}
		})
	}
}
