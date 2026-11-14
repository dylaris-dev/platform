package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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

// editionsCountingStore counts the publishes a Bedrock audience save starts.
type editionsCountingStore struct {
	*gateFakeStore
	publishes int
}

func (s *editionsCountingStore) ListRouteEditions(int) ([]store.RouteEdition, error) {
	s.publishes++
	return nil, nil
}
func (s *editionsCountingStore) ListLinkRouteEditions() ([]store.LinkRouteEdition, error) {
	return nil, nil
}
func (s *editionsCountingStore) ListBedrockServerRefs() ([]store.BedrockServerRef, error) {
	return nil, nil
}

// Final review M-6: on the managed gateway the provider publishes editions and
// keys. Saving the Bedrock audience then must not publish them into this
// platform's own Redis, where a local leftover row could even mint a local
// Floodgate key; local mode still publishes at once.
func TestBedrockAudienceSaveSkipsThePublishInRemoteMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      func() (bool, bool)
		publishes int
	}{
		{"remote", func() (bool, bool) { return true, true }, 0},
		{"mode unread", func() (bool, bool) { return false, false }, 0},
		{"local", func() (bool, bool) { return false, true }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := newFeatureSettingsFixture(t, nil)
			st := &editionsCountingStore{gateFakeStore: fake}
			h.d.Store = st
			h.d.Redis = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			h.d.GatewayRemote = tc.mode
			rec := httptest.NewRecorder()
			h.Set(rec, httptest.NewRequest(http.MethodPut, "/api/admin/settings/features", strings.NewReader(`{"bedrock":"all"}`)))
			if rec.Code != http.StatusOK || fake.kv[services.BedrockSettingKey] != "all" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if st.publishes != tc.publishes {
				t.Fatalf("publishes %d, want %d", st.publishes, tc.publishes)
			}
		})
	}
}
