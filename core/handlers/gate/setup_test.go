package gate

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/store"
)

type setupCountStore struct {
	store.Store
	users int
	err   error
}

func (s setupCountStore) HasUsers() (bool, error) { return s.users > 0, s.err }

// The setup lock moved here from AppState (S13). It asks only "is there any
// user": a fresh install refuses everything but the wizard, any install with
// users passes (the auth middleware decides from there), and a store error
// locks rather than opening the API.
func TestRequireSetupCompleteLocksOnlyAFreshInstall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		users      int
		err        error
		path       string
		wantLocked bool
	}{
		{"fresh install locks the API", 0, nil, "/api/servers", true},
		{"fresh install keeps the wizard open", 0, nil, "/api/setup/status", false},
		{"users present pass (complete or lost-admin)", 2, nil, "/api/servers", false},
		{"a store error locks", 0, errors.New("db down"), "/api/servers", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := Gates{Store: setupCountStore{users: tc.users, err: tc.err}}.RequireSetupComplete(func(w http.ResponseWriter, r *http.Request) {
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
