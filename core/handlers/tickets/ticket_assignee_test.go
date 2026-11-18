package tickets

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// assigneeStore serves an admin caller and one candidate assignee whose
// support status may come from a PANEL role rather than users.role.
type assigneeStore struct {
	store.Store
	users    map[string]*models.User
	roles    map[string]*store.PanelRole
	assigned bool
	ticket   *models.Ticket
	settings map[string]string
	removed  bool
}

func (f *assigneeStore) GetUserByID(id string) (*models.User, error) { return f.users[id], nil }
func (f *assigneeStore) GetUserPanelAuthz(id string) (*int, store.CapOverrides, error) {
	r := f.roles[id]
	if r == nil {
		return nil, store.CapOverrides{}, nil
	}
	rid := r.ID
	return &rid, store.CapOverrides{}, nil
}
func (f *assigneeStore) GetPanelRole(id int) (*store.PanelRole, error) {
	for _, r := range f.roles {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, nil
}
func (f *assigneeStore) GetTicket(id int) (*models.Ticket, error) {
	if f.ticket != nil {
		return f.ticket, nil
	}
	return &models.Ticket{ID: id, UserID: "owner"}, nil
}
func (f *assigneeStore) IsTicketWatcher(int, string) (bool, error) { return false, nil }
func (f *assigneeStore) GetSetting(k string) (string, error)       { return f.settings[k], nil }
func (f *assigneeStore) RemoveTicketWatcher(int, string) (bool, error) {
	f.removed = true
	return true, nil
}
func (f *assigneeStore) UpdateTicketAssignment(int, *string, string) error {
	f.assigned = true
	return nil
}
func (f *assigneeStore) InsertTicketAudit(*models.TicketAuditEvent) error { return nil }
func (f *assigneeStore) InsertNotification(*models.Notification) (int64, error) {
	return 1, nil
}

// The assignee must be judged by the same effective permissions every other
// ticket gate uses. An account holding the seeded "support" PANEL role sees
// the inbox and may reply, so it must also be assignable.
func TestAssigneeIsJudgedByEffectivePermissions(t *testing.T) {
	supportRole := &store.PanelRole{ID: 2, Name: "support", Capabilities: []string{"tickets.read", "tickets.write"}}
	cases := []struct {
		name   string
		target *models.User
		role   *store.PanelRole
		want   int
	}{
		{"panel-role supporter", &models.User{ID: "t", Role: "user"}, supportRole, http.StatusOK},
		{"legacy support role", &models.User{ID: "t", Role: "support"}, nil, http.StatusOK},
		{"admin", &models.User{ID: "t", IsAdmin: true}, nil, http.StatusOK},
		{"plain user", &models.User{ID: "t", Role: "user"}, nil, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &assigneeStore{
				users: map[string]*models.User{"admin": {ID: "admin", IsAdmin: true}, "t": c.target},
				roles: map[string]*store.PanelRole{},
			}
			if c.role != nil {
				fs.roles["t"] = c.role
			}
			h := &TicketsHandler{d: Deps{Store: fs, Authz: authz.NewResolver(fs)}}
			r := httptest.NewRequest(http.MethodPatch, "/api/tickets/1/assignment", bytes.NewBufferString(`{"assignedUserId":"t"}`))
			r = r.WithContext(context.WithValue(r.Context(), "userID", "admin"))
			w := httptest.NewRecorder()
			h.UpdateAssignment(w, r)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if fs.assigned != (c.want == http.StatusOK) {
				t.Errorf("assigned = %v", fs.assigned)
			}
		})
	}
}

// RemoveWatcher takes the same visibility gate as every other ticket mutation:
// a supporter confined to team A must not remove watchers from (or probe) a
// team-B ticket. Removing yourself stays allowed, it only reaches your own row.
func TestRemoveWatcherChecksTicketVisibility(t *testing.T) {
	const sUID, wUID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	writer := &store.PanelRole{ID: 2, Name: "support", Capabilities: []string{"tickets.read", "tickets.write"}}
	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"someone else on a foreign team's ticket", wUID, http.StatusNotFound},
		{"yourself", sUID, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &assigneeStore{
				users: map[string]*models.User{
					sUID: {ID: sUID, Role: "user", SupportTeam: "a"},
					wUID: {ID: wUID},
				},
				roles:    map[string]*store.PanelRole{sUID: writer},
				ticket:   &models.Ticket{ID: 1, UserID: "owner", AssignedTeam: "b"},
				settings: map[string]string{"tickets.cross_team_visibility": "false"},
			}
			h := &TicketsHandler{d: Deps{Store: fs, Authz: authz.NewResolver(fs)}}
			r := httptest.NewRequest(http.MethodDelete, "/api/tickets/1/watchers/"+c.target, nil)
			r = mux.SetURLVars(r, map[string]string{"id": "1", "userId": c.target})
			r = r.WithContext(context.WithValue(r.Context(), "userID", sUID))
			w := httptest.NewRecorder()
			h.RemoveWatcher(w, r)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if fs.removed != (c.want == http.StatusOK) {
				t.Errorf("removed = %v", fs.removed)
			}
		})
	}
}
