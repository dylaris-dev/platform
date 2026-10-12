package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type noInvitesStore struct{ store.Store }

func (noInvitesStore) ListInvitesByServer(int) ([]models.ServerInvite, error) { return nil, nil }

// A server with no members answered "members": null, which a client calling
// .length or .map on it falls over.
func TestNoMembersIsAnEmptyList(t *testing.T) {
	h := &MemberHandler{state: &AppState{Store: noInvitesStore{}}}
	req := mux.SetURLVars(httptest.NewRequest("GET", "/api/servers/4/members", nil), map[string]string{"id": "4"})
	rec := httptest.NewRecorder()
	h.GetMembers(rec, req)
	if !strings.Contains(rec.Body.String(), `"members":[]`) {
		t.Fatalf("body = %s, want an empty members list", rec.Body.String())
	}
}
