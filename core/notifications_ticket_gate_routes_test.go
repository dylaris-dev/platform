package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
)

// notificationGateFakeStore answers the handful of calls the notification
// routes make. It embeds the nil store.Store, so anything else panics - which
// the harness turns into a 500 and the assertions below then report.
type notificationGateFakeStore struct {
	authzFakeStore
}

func (f *notificationGateFakeStore) ListNotifications(userID string, includeRead bool, limit int) ([]models.Notification, error) {
	return nil, nil
}
func (f *notificationGateFakeStore) CountUnreadNotifications(userID string) (int, error) {
	return 0, nil
}
func (f *notificationGateFakeStore) MarkNotificationRead(id int64, userID string) error { return nil }
func (f *notificationGateFakeStore) MarkAllNotificationsRead(userID string) error       { return nil }

// The inbox is a PERSONAL one, not a view of the ticket system, and it must
// keep answering when the ticket feature is switched off.
//
// It did not. The inbox began as ticket-driven and inherited RequireTicketsEnabled
// on all four routes, then grew notifications that have nothing to do with
// tickets - server.install_failed is one, and it is the only way an owner learns
// their server did not install. An operator turning tickets off was turning that
// off too, with nothing to say so.
//
// This drives the REAL router, so it fails again if somebody wraps these routes
// in the ticket gate a second time. The positive control in the same test keeps
// it honest: a genuine ticket route MUST still be gated, or an assertion that
// nothing is gated would pass on a build where the gate stopped working.
func TestNotificationsAreNotGatedOnTheTicketFeature(t *testing.T) {
	// feature_tickets_enabled unset = off (the flag defaults to false).
	fs := &notificationGateFakeStore{authzFakeStore{settings: map[string]string{}}}
	// A REGISTERED user, so every request below is rejected on its own merits
	// rather than at authentication - a 401 everywhere would satisfy "not
	// gated" while proving nothing.
	fs.addUser("u1", "alice", false)
	srv := newCoreStorageGateTestServer(t, fs)
	token := mintToken(t, testIdentity{UserID: "u1", Username: "alice"})

	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	gated := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusServiceUnavailable &&
			rec.Header().Get("X-Feature-Disabled") == handlers.FeatureTickets
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/notifications"},
		{http.MethodGet, "/api/notifications/unread-count"},
		{http.MethodPost, "/api/notifications/1/read"},
		{http.MethodPost, "/api/notifications/read-all"},
	} {
		rec := call(tc.method, tc.path)
		if gated(rec) {
			t.Errorf("%s %s answered the ticket gate with tickets off; an owner would never see that their server failed to install",
				tc.method, tc.path)
		}
	}

	// Positive control: a route that really is part of the ticket system. It is
	// reachable by any authenticated user (a customer's own tickets), so a
	// non-gated build answers it rather than refusing on capabilities.
	if rec := call(http.MethodGet, "/api/tickets"); !gated(rec) {
		t.Errorf("GET /api/tickets was NOT gated with tickets off (%d, X-Feature-Disabled=%q); "+
			"without this the assertions above would pass on a build where the gate is simply broken",
			rec.Code, rec.Header().Get("X-Feature-Disabled"))
	}
}
