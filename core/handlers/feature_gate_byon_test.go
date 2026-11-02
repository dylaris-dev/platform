package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
)

// TestFeatureSettings_ByonRoundTrip drives PUT then GET through
// FeatureSettingsHandler and asserts the new byon field is persisted and read
// back alongside the existing toggles. tickets=false/autoMove=false keep the
// PUT clear of the storage + gateway guards.
func TestFeatureSettings_ByonRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		body string
		// The four flags this test drives, read back as plain booleans. The
		// payload carries pointers now (absent means "not part of this
		// request"), so comparing the structs would compare addresses.
		want map[string]bool
	}{
		{
			"byon on, modpacks on",
			`{"tickets":false,"modpacks":true,"autoMove":false,"byon":true}`,
			map[string]bool{"tickets": false, "modpacks": true, "autoMove": false, "byon": true},
		},
		{
			"byon off, all off",
			`{"tickets":false,"modpacks":false,"autoMove":false,"byon":false}`,
			map[string]bool{"tickets": false, "modpacks": false, "autoMove": false, "byon": false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &AppState{
				Store:  &gateFakeStore{kv: map[string]string{}},
				Events: services.NewSystemEventsPublisher(nil),
			}
			st.FeatureFlags = services.NewFeatureFlags(st.Store)
			h := NewFeatureSettingsHandler(st)

			putRW := httptest.NewRecorder()
			h.Set(putRW, httptest.NewRequest(http.MethodPut, "/api/admin/settings/features", strings.NewReader(c.body)))
			if putRW.Code != http.StatusOK {
				t.Fatalf("PUT status = %d, want 200 (%s)", putRW.Code, putRW.Body.String())
			}

			getRW := httptest.NewRecorder()
			h.Get(getRW, httptest.NewRequest(http.MethodGet, "/api/admin/settings/features", nil))
			if getRW.Code != http.StatusOK {
				t.Fatalf("GET status = %d, want 200", getRW.Code)
			}
			var resp struct {
				Success  bool                   `json:"success"`
				Features featureSettingsPayload `json:"features"`
			}
			if err := json.Unmarshal(getRW.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode GET: %v", err)
			}
			// GET always states every flag, so a nil here is itself a failure.
			got := map[string]*bool{
				"tickets":  resp.Features.Tickets,
				"modpacks": resp.Features.Modpacks,
				"autoMove": resp.Features.AutoMove,
				"byon":     resp.Features.Byon,
			}
			for name, want := range c.want {
				if got[name] == nil {
					t.Fatalf("GET omitted %q; it must state every flag", name)
				}
				if *got[name] != want {
					t.Fatalf("round-trip %s = %v, want %v", name, *got[name], want)
				}
			}
		})
	}
}
