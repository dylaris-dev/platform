package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// Replacing your own member permissions dropped the owner's denies on your
// grant. The handler refuses a member's own row before anything else.
func TestAMemberCannotReplaceTheirOwnAccess(t *testing.T) {
	const me = "22222222-2222-2222-2222-222222222222"
	h := &MemberHandler{state: &AppState{Store: &setupFakeStore{}}}
	req := httptest.NewRequest("PATCH", "/api/servers/5/members/"+me, bytes.NewReader([]byte(`{"permissions":{}}`)))
	req = mux.SetURLVars(req, map[string]string{"id": "5", "userId": me})
	req = req.WithContext(context.WithValue(req.Context(), "userID", me))
	rec := httptest.NewRecorder()
	h.UpdateMemberPermissions(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// tickets.write alone let a supporter limited to their team act on any
// ticket. Every mutating ticket handler checks the ticket's visibility first.
func TestTicketMutationsCheckVisibility(t *testing.T) {
	for _, fn := range []string{"AddReply", "UpdateStatus", "UpdatePriority", "UpdateAssignment", "AddWatcher"} {
		body := funcBody(t, "tickets.go", fn)
		see := strings.Index(body, "if !callerSeesTicket(h.state, t, perms, userID) {")
		decode := strings.Index(body, "json.NewDecoder(r.Body)")
		if see < 0 || (decode >= 0 && see > decode) {
			t.Errorf("%s acts on a ticket without checking the caller can see it", fn)
		}
	}
	if del := funcBody(t, "ticket_attachments.go", "DeleteAttachment"); strings.Contains(del, "perms.IsSupport ||") {
		t.Error("read-only support may still delete attachments")
	}
}

// Ticket creation and add-watcher notify other users; both are rate limited.
func TestTicketNotifyingRoutesAreRateLimited(t *testing.T) {
	b, err := os.ReadFile("../routes.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"ticketLimiter.Limit(10, ticketsHandler.CreateTicket)", "watcherLimiter.Limit(20, ticketsHandler.AddWatcher)"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s", want)
		}
	}
}
