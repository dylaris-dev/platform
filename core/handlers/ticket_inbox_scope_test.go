package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// inboxScopeStore records the filter the inbox handed the store, which is the
// thing under test: what the QUERY asked for, not what survived the post-filter.
type inboxScopeStore struct {
	store.Store

	tickets   []models.Ticket
	gotFilter store.TicketFilter
	user      *models.User
}

func (f *inboxScopeStore) ListTickets(filter store.TicketFilter) ([]models.Ticket, error) {
	f.gotFilter = filter
	return f.tickets, nil
}
func (f *inboxScopeStore) GetUserByID(string) (*models.User, error) { return f.user, nil }
func (f *inboxScopeStore) GetSetting(string) (string, error)        { return "", nil }

const inboxAdmin = "aaaaaaaa-1111-4111-8111-111111111111"

func inboxRequest(t *testing.T, fs *inboxScopeStore, scope string) []models.Ticket {
	t.Helper()
	h := &TicketsHandler{state: &AppState{Store: fs}}
	r := httptest.NewRequest(http.MethodGet, "/api/tickets/inbox?scope="+scope, nil)
	r = r.WithContext(context.WithValue(r.Context(), "userID", inboxAdmin))
	w := httptest.NewRecorder()
	h.ListInboxTickets(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Tickets []models.Ticket `json:"tickets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Tickets
}

// scope=unassigned must not ask the database for a TEAM named "__unassigned__".
//
// It used to: AssignedTeam goes straight into "t.assigned_team = $n", so the
// query asked for tickets belonging to a team nobody has. No row ever matched,
// and the post-filter that was supposed to do the real work can only narrow
// what the query returned - so the triage queue was empty for everyone, admins
// included.
func TestTheUnassignedInboxScopeDoesNotFilterOnASentinelTeam(t *testing.T) {
	unassigned := models.Ticket{ID: 1}
	assignedToTeam := models.Ticket{ID: 2, AssignedTeam: "billing"}
	fs := &inboxScopeStore{
		tickets: []models.Ticket{unassigned, assignedToTeam},
		user:    &models.User{ID: inboxAdmin, IsAdmin: true},
	}

	got := inboxRequest(t, fs, "unassigned")

	if fs.gotFilter.AssignedTeam != "" {
		t.Errorf("the query filtered on assigned_team = %q, which no ticket carries", fs.gotFilter.AssignedTeam)
	}
	if len(got) != 1 || got[0].ID != unassigned.ID {
		t.Fatalf("unassigned scope returned %d ticket(s), want exactly the unassigned one: %+v", len(got), got)
	}
}

// The other scopes still narrow in the store, where they belong.
func TestTheOtherInboxScopesStillFilterInTheStore(t *testing.T) {
	fs := &inboxScopeStore{user: &models.User{ID: inboxAdmin, IsAdmin: true, SupportTeam: "billing"}}

	inboxRequest(t, fs, "mine")
	if fs.gotFilter.AssignedUserID == nil || *fs.gotFilter.AssignedUserID != inboxAdmin {
		t.Errorf("scope=mine did not filter on the caller: %+v", fs.gotFilter.AssignedUserID)
	}

	fs.gotFilter = store.TicketFilter{}
	inboxRequest(t, fs, "team")
	if fs.gotFilter.AssignedTeam != "billing" {
		t.Errorf("scope=team filtered on %q, want the caller's team", fs.gotFilter.AssignedTeam)
	}
}
