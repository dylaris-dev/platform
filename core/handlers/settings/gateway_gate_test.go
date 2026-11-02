package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The feature-settings payload must not carry a gateway flag. Re-adding one
// re-creates the split brain: the panel would gate its routes UI on a value no
// gate in Core ever reads.
func TestFeatureSettingsCarriesNoGatewayFlag(t *testing.T) {
	st := Deps{Store: &gateFakeStore{kv: map[string]string{
		"routing_mode": "gateway",
	}}}
	h := NewSettingsHandler(st)

	rw := httptest.NewRecorder()
	h.GetFeatureSettings(rw, httptest.NewRequest(http.MethodGet, "/api/settings/features", nil))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if strings.Contains(strings.ToLower(rw.Body.String()), "gateway") {
		t.Fatalf("feature settings mention a gateway flag: %s", rw.Body.String())
	}

	var got struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.Settings["proxyEnabled"]; !ok {
		t.Fatalf("proxyEnabled missing from %v", got.Settings)
	}
	if len(got.Settings) != 1 {
		t.Fatalf("settings = %v, want only proxyEnabled", got.Settings)
	}
}
