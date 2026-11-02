package gate

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/store"
)

type setupCountStore struct {
	store.Store
	admins, users int
}

func (s setupCountStore) CountAdmins() (int, error) { return s.admins, nil }
func (s setupCountStore) CountUsers() (int, error)  { return s.users, nil }

// The setup lock moved here from AppState (S13) and had no test of its own.
// Pin the three install states: a fresh install refuses everything but the
// wizard, and both a lost-admin install and a complete one pass through (the
// auth middleware decides from there).
func TestRequireSetupCompleteLocksOnlyAFreshInstall(t *testing.T) {
	for _, tc := range []struct {
		name          string
		admins, users int
		path          string
		wantLocked    bool
	}{
		{"fresh install locks the API", 0, 0, "/api/servers", true},
		{"fresh install keeps the wizard open", 0, 0, "/api/setup/status", false},
		{"users but no admin pass", 0, 2, "/api/servers", false},
		{"an admin passes", 1, 1, "/api/servers", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := Gates{Store: setupCountStore{admins: tc.admins, users: tc.users}}.RequireSetupComplete(func(w http.ResponseWriter, r *http.Request) {
				called = true
			})
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			locked := rec.Code == http.StatusServiceUnavailable && rec.Header().Get("X-Setup-Required") == "1"
			if locked != tc.wantLocked || called == tc.wantLocked {
				t.Fatalf("locked=%v called=%v (status %d), want locked=%v", locked, called, rec.Code, tc.wantLocked)
			}
		})
	}
}
