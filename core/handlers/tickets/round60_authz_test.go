package tickets_test

import (
	"os"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
)

// tickets.write alone let a supporter limited to their team act on any
// ticket. Every mutating ticket handler checks the ticket's visibility first.
func TestTicketMutationsCheckVisibility(t *testing.T) {
	for _, fn := range []string{"AddReply", "UpdateStatus", "UpdatePriority", "UpdateAssignment", "AddWatcher"} {
		body := handlertest.FuncBody(t, "tickets.go", fn)
		see := strings.Index(body, "if !callerSeesTicket(h.d, t, perms, userID) {")
		decode := strings.Index(body, "json.NewDecoder(r.Body)")
		if see < 0 || (decode >= 0 && see > decode) {
			t.Errorf("%s acts on a ticket without checking the caller can see it", fn)
		}
	}
	if del := handlertest.FuncBody(t, "ticket_attachments.go", "DeleteAttachment"); strings.Contains(del, "perms.IsSupport ||") {
		t.Error("read-only support may still delete attachments")
	}
}

// Ticket creation and add-watcher notify other users; both are rate limited.
func TestTicketNotifyingRoutesAreRateLimited(t *testing.T) {
	b, err := os.ReadFile("../../routes.go")
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
