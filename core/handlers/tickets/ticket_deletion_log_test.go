package tickets

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/handlertest"
)

// The deletion log holds every deleted ticket's subject and owner across all
// teams plus the deleting admin's IP and user agent. tickets.read is a team-
// limited supporter's capability, so the log is admin-only like the delete.
func TestTheDeletionLogIsAdminOnly(t *testing.T) {
	rw := httptest.NewRecorder()
	(&TicketDeletionsHandler{}).ListDeletions(rw, handlertest.StaffRequest("GET", nil, ""))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("staff read the deletion log: status %d", rw.Code)
	}
}
