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

type inviteInputStore struct {
	store.Store
	created bool
}

func (f *inviteInputStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, OwnerID: "owner-1", OwnerName: "owner"}, nil
}
func (f *inviteInputStore) GetUserByUsername(name string) (*models.User, error) {
	return &models.User{ID: "guest-1", Username: name}, nil
}
func (f *inviteInputStore) CreateInvite(serverID int, userID, inviterID string, perms map[string]bool) error {
	f.created = true
	return nil
}

// The audit trail switches itself on at the first invite and writes a row for
// it. Neither is what these cases are about, so both answer harmlessly.
func (f *inviteInputStore) GetServerAuditState(int) (bool, bool, int, error) {
	return true, false, 0, nil
}
func (f *inviteInputStore) InsertServerAudit(*models.ServerAuditEvent) error { return nil }

func inviteReq(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/servers/1/members", bytes.NewReader([]byte(body)))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "userID", "owner-1")
	ctx = context.WithValue(ctx, "username", "owner")
	ctx = context.WithValue(ctx, "isAdmin", false)
	return r.WithContext(ctx)
}

// This endpoint decides what somebody may do to a server, and it used to answer
// two different mistakes with "success". Measured on production: an invite sent
// as {"username": ..., "preset": "viewer"} dropped the word "viewer" on the
// floor, fell through to a default that granted everything but member
// management, and the "viewer" could then write files and stop the server.
//
// So: no permissions is a refusal, an unknown FIELD is a refusal, and an
// unknown permission KEY is a refusal. Each one says what was wrong.
func TestInviteRefusesAnythingItDoesNotUnderstand(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCreate bool
	}{
		{
			name:       "a body with no permissions at all",
			body:       `{"username":"guest"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a field this endpoint does not know",
			body:       `{"username":"guest","preset":"viewer"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a permission key that means nothing",
			body:       `{"username":"guest","permissions":{"console":true,"sudo":true}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			// The way to say "may see it and nothing else" has to exist, or the
			// refusal above just moves the problem.
			name:       "an empty permission set is a valid answer",
			body:       `{"username":"guest","permissions":{}}`,
			wantStatus: http.StatusOK,
			wantCreate: true,
		},
		{
			name:       "an explicit read-only set",
			body:       `{"username":"guest","permissions":{"overview":true,"console":true,"files":false,"power":false}}`,
			wantStatus: http.StatusOK,
			wantCreate: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &inviteInputStore{}
			h := NewMemberHandler(&AppState{Store: fs})
			rec := httptest.NewRecorder()

			h.InviteMember(rec, inviteReq(c.body))

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if fs.created != c.wantCreate {
				t.Fatalf("invite created = %v, want %v", fs.created, c.wantCreate)
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

// The vocabulary itself: every key the invite blob can carry has to be
// accepted, or the refusal above turns into a wall in front of a legitimate
// caller.
func TestUnknownPermissionKeys(t *testing.T) {
	all := map[string]bool{}
	for _, k := range invitePermissionKeys {
		all[k] = true
	}
	if bad := unknownPermissionKeys(all); len(bad) != 0 {
		t.Errorf("the documented vocabulary rejects its own keys: %v", bad)
	}
	bad := unknownPermissionKeys(map[string]bool{"console": true, "zzz": true, "aaa": false})
	if len(bad) != 2 || bad[0] != "aaa" || bad[1] != "zzz" {
		t.Errorf("unknown keys = %v, want [aaa zzz] sorted so the message is stable", bad)
	}
}
