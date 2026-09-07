package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The billing screens set money-shaped policy, and without a hosted store there
// is nobody to bill. The panel has always hidden them on `requiresStore`, but
// the API answered on `RequireBYONEnabled` alone - so the endpoints behind a
// page a self-hosted operator cannot open were still live to anything that knew
// the path, and their settings still governed a real gate.
func TestRequireStoreEnabled(t *testing.T) {
	for _, tt := range []struct {
		name       string
		enabled    bool
		wantStatus int
		wantCalled bool
	}{
		{name: "linked to the store, the handler runs", enabled: true, wantStatus: http.StatusOK, wantCalled: true},
		{name: "self-hosted, refused as a disabled feature", enabled: false, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			s := &AppState{StoreEnabled: tt.enabled}
			h := s.RequireStoreEnabled(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings/billing", nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if called != tt.wantCalled {
				t.Errorf("handler called = %v, want %v", called, tt.wantCalled)
			}
			if !tt.wantCalled {
				// The panel keys its "this feature is off" handling off the
				// header, not the body, so a refusal without it reads as a
				// server fault instead.
				if got := rec.Header().Get("X-Feature-Disabled"); got != FeatureStore {
					t.Errorf("X-Feature-Disabled = %q, want %q", got, FeatureStore)
				}
			}
		})
	}
}
