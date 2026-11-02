package servers

import (
	"bytes"
	"context"
	"dylaris-core/handlers"
	"dylaris-core/store"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// Replacing your own member permissions dropped the owner's denies on your
// grant. The handler refuses a member's own row before anything else.
func TestAMemberCannotReplaceTheirOwnAccess(t *testing.T) {
	const me = "22222222-2222-2222-2222-222222222222"
	// The refusal comes before any store read, so an empty store suffices.
	h := &MemberHandler{state: withCoreStorage(&handlers.AppState{Store: struct{ store.Store }{}})}
	req := httptest.NewRequest("PATCH", "/api/servers/5/members/"+me, bytes.NewReader([]byte(`{"permissions":{}}`)))
	req = mux.SetURLVars(req, map[string]string{"id": "5", "userId": me})
	req = req.WithContext(context.WithValue(req.Context(), "userID", me))
	rec := httptest.NewRecorder()
	h.UpdateMemberPermissions(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
	}
}
