package tickets

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
)

// tickets.write is the support role, and this screen turns off the team
// isolation support is held in and shortens the audit trail that watches it.
func TestTheTicketPolicyIsTheOperators(t *testing.T) {
	rw := httptest.NewRecorder()
	(&TicketSettingsHandler{state: &handlers.AppState{}}).SaveSettings(rw,
		staffRequest("PUT", nil, `{"crossTeamVisibility":true,"auditRetentionDays":1}`))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("staff changed the ticket policy: status %d", rw.Code)
	}
}
