package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// solderTenancyStore answers the handle lookup and nothing else.
type solderTenancyStore struct {
	store.Store
	handles map[string]string // handle -> ownerID
}

func (f *solderTenancyStore) GetSetting(key string) (string, error) {
	if key == "feature_modpacks_enabled" {
		return "true", nil
	}
	return "", nil
}

func (f *solderTenancyStore) GetUserIDBySolderHandle(handle string) (string, error) {
	return f.handles[handle], nil
}

// An unknown handle is an address with nothing behind it, and the launcher
// contract answers that as 404 in the Solder shape - never 403, which is the one
// status this API reserves for a bad key. Telling "no such account" apart from
// "wrong key" from outside is what would let somebody enumerate handles.
func TestSolderScopeRejectsAnUnknownHandle(t *testing.T) {
	st := &solderTenancyStore{handles: map[string]string{"bartis": solderTestOwner}}
	h := &SolderHandler{state: &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}}

	for _, tt := range []struct {
		name, handle string
		wantStatus   int
	}{
		{name: "a claimed handle", handle: "bartis", wantStatus: http.StatusOK},
		{name: "nobody's handle", handle: "someone-else", wantStatus: http.StatusNotFound},
		// Every account starts with an empty handle, so resolving "" would
		// otherwise match all of them.
		{name: "the empty handle", handle: "", wantStatus: http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/solder/u/x/api/", nil),
				map[string]string{"handle": tt.handle})
			h.Info(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// The retired shared URL must answer in the Solder shape. An operator who typed
// the old address is exactly the person who needs to read where it went, and an
// HTML 404 is what made a missing trailing slash so hard to diagnose one release
// ago.
func TestLegacySolderRootExplainsWhereItWent(t *testing.T) {
	h := &SolderHandler{state: &AppState{}}
	rec := httptest.NewRecorder()
	h.LegacyAPI(rec, httptest.NewRequest(http.MethodGet, "/solder/api/", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "/solder/u/") {
		t.Errorf("body does not name the new address: %s", body)
	}
}
