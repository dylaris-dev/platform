package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireSetupCompleteGatesOnAnyUser(t *testing.T) {
	cases := []struct {
		name  string
		users int
		err   error
		path  string
		want  int
	}{
		{"fresh install locks", 0, nil, "/api/servers", http.StatusServiceUnavailable},
		{"fresh install keeps setup open", 0, nil, "/api/setup/status", http.StatusOK},
		{"users present passes (complete or lost-admin)", 3, nil, "/api/servers", http.StatusOK},
		{"store error locks", 0, errors.New("db down"), "/api/servers", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &AppState{Store: &setupFakeStore{userCount: tc.users, countErr: tc.err}}
			h := s.RequireSetupComplete(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
