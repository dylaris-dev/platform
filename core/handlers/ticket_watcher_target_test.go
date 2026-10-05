package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// ticketWatcherFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods AddWatcher reaches before it
// answers are overridden. Any other call would panic - this test never gets
// far enough to make one.
type ticketWatcherFakeStore struct {
	store.Store

	users  map[string]*models.User
	ticket *models.Ticket
}

func (f *ticketWatcherFakeStore) GetUserByID(id string) (*models.User, error) {
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, nil
}

func (f *ticketWatcherFakeStore) GetUserByUsername(name string) (*models.User, error) {
	for _, u := range f.users {
		if u.Username == name {
			return u, nil
		}
	}
	return nil, nil
}

func (f *ticketWatcherFakeStore) GetUserRegionIDs(string) ([]string, error) { return nil, nil }
func (f *ticketWatcherFakeStore) GetSetting(string) (string, error)         { return "", nil }
func (f *ticketWatcherFakeStore) GetTicket(int) (*models.Ticket, error)     { return f.ticket, nil }
func (f *ticketWatcherFakeStore) IsTicketWatcher(int, string) (bool, error) { return false, nil }

// A userId that names nobody used to reach the insert and come back as a
// foreign-key violation dressed up as a 500 "Failed to add watcher". The
// username branch right above it has always answered 404 for the same
// mistake; both branches now say the same thing.
func TestAddWatcherRejectsAnUnknownUser(t *testing.T) {
	const adminID = "11111111-1111-1111-1111-111111111111"
	fs := &ticketWatcherFakeStore{
		users: map[string]*models.User{
			adminID: {ID: adminID, Username: "admin", IsAdmin: true, Role: "admin"},
		},
		ticket: &models.Ticket{ID: 4, UserID: adminID},
	}
	h := &TicketsHandler{state: &AppState{Store: fs}}

	tests := []struct {
		name     string
		body     string
		wantCode int
		wantBody string
	}{
		{"unknown userId", `{"userId":"00000000-0000-0000-0000-000000000000"}`, http.StatusNotFound, "User not found"},
		{"unknown username", `{"username":"ghost"}`, http.StatusNotFound, "User not found"},
		{"neither field", `{}`, http.StatusBadRequest, "userId or username required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/tickets/4/watchers", bytes.NewBufferString(tt.body))
			req = mux.SetURLVars(req, map[string]string{"id": "4"})
			req = req.WithContext(context.WithValue(req.Context(), "userID", adminID)) //nolint:staticcheck // matches the middleware's string key
			rec := httptest.NewRecorder()

			h.AddWatcher(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %s, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
