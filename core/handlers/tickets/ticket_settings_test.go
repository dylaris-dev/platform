package tickets

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/services/corestorage"
)

// tickets.write is the support role, and this screen turns off the team
// isolation support is held in and shortens the audit trail that watches it.
func TestTheTicketPolicyIsTheOperators(t *testing.T) {
	rw := httptest.NewRecorder()
	(&TicketSettingsHandler{d: Deps{CoreStorage: &corestorage.Manager{}}}).SaveSettings(rw,
		handlertest.StaffRequest("PUT", nil, `{"crossTeamVisibility":true,"auditRetentionDays":1}`))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("staff changed the ticket policy: status %d", rw.Code)
	}
}

// A quota above what LoadTicketSettings honours was stored and echoed as
// saved, then silently read back as the product default. Save refuses it.
func TestSaveSettingsRefusesQuotasTheReaderIgnores(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"file at max", `{"maxFileSizeMb":1024}`, http.StatusOK},
		{"file over max", `{"maxFileSizeMb":2048}`, http.StatusBadRequest},
		{"ticket over max", `{"maxTicketSizeMb":20000}`, http.StatusBadRequest},
		{"user over max", `{"maxUserSizeMb":150000}`, http.StatusBadRequest},
		{"no cap", `{"maxUserSizeMb":null}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := handlertest.NewSettingsStore()
			rw := httptest.NewRecorder()
			(&TicketSettingsHandler{d: Deps{Store: st}}).SaveSettings(rw, handlertest.AdminRequest("PUT", nil, c.body))
			if rw.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rw.Code, c.want, rw.Body.String())
			}
		})
	}
}
