package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BYON and route-only cannot be switched ON while the managed gateway is the
// provider, and a false sent then is not written either: the getters answer
// false in remote mode, so the panel sends that back and would otherwise
// overwrite the stored choice. Every local save still works.
func TestRemoteModeRefusesTenancyFlags(t *testing.T) {
	mode := func(remote, known bool) func() (bool, bool) { return func() (bool, bool) { return remote, known } }
	tests := []struct {
		name    string
		mode    func() (bool, bool)
		body    string
		status  int
		code    string
		key     string
		written string
	}{
		{"remote byon on", mode(true, true), `{"byon":true}`, http.StatusConflict, "gateway_remote", "feature_byon_enabled", "true"},
		{"remote route-only on", mode(true, true), `{"routeOnly":true}`, http.StatusConflict, "gateway_remote", "feature_route_only_enabled", "true"},
		{"remote byon off keeps the stored true", mode(true, true), `{"byon":false,"routeOnly":false}`, http.StatusOK, "", "feature_byon_enabled", "true"},
		{"remote route-only off keeps the stored true", mode(true, true), `{"byon":false,"routeOnly":false}`, http.StatusOK, "", "feature_route_only_enabled", "true"},
		{"unknown byon off is written", mode(false, false), `{"byon":false}`, http.StatusOK, "", "feature_byon_enabled", "false"},
		{"unknown byon on", mode(false, false), `{"byon":true}`, http.StatusServiceUnavailable, "gateway_provider_unknown", "feature_byon_enabled", "true"},
		{"local byon off", mode(false, true), `{"byon":false}`, http.StatusOK, "", "feature_byon_enabled", "false"},
		{"no mode wired", nil, `{"routeOnly":false}`, http.StatusOK, "", "feature_route_only_enabled", "false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, st := newFeatureSettingsFixture(t, map[string]string{"feature_byon_enabled": "true", "feature_route_only_enabled": "true"})
			h.d.GatewayRemote = tt.mode
			rec := httptest.NewRecorder()
			h.Set(rec, httptest.NewRequest(http.MethodPut, "/api/admin/settings/features", strings.NewReader(tt.body)))
			if rec.Code != tt.status {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tt.status, rec.Body)
			}
			if tt.code != "" {
				var body struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if body.Code != tt.code {
					t.Fatalf("code %q, want %q", body.Code, tt.code)
				}
			}
			if got := st.kv[tt.key]; got != tt.written {
				t.Fatalf("%s = %q after the request, want %q", tt.key, got, tt.written)
			}
		})
	}
}
