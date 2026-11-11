package billing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
)

// A user's "*" edge row is the managed-gateway product, written only by the
// store. An admin write of that shape (create, update or delete) is refused,
// and the row the store wrote is left as it was; every other shape still saves.
func TestAdminCannotWriteTheStoreOwnedManagedRow(t *testing.T) {
	inc := int64(5000)
	put := func(h *TrafficLimitHandler, body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h.SetTrafficLimit(rec, httptest.NewRequest(http.MethodPut, "/api/traffic-limits", bytes.NewReader(b)))
		return rec
	}
	custom := func(scope, region, kind string) map[string]any {
		return map[string]any{"scope": scope, "region": region, "kind": kind,
			"includedMode": "custom", "includedGb": 100, "purchaseMode": "custom", "purchaseGb": 50}
	}
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"create or update", custom("user:u1", "*", "edge"), http.StatusBadRequest},
		{"delete", map[string]any{"scope": "user:u1", "region": "*", "kind": "edge",
			"includedMode": "default", "purchaseMode": "default"}, http.StatusBadRequest},
		{"user per-region edge row", custom("user:u1", "eu-central", "edge"), http.StatusOK},
		{"user relay row", custom("user:u1", "*", "relay"), http.StatusOK},
		{"platform default * edge", custom("user_default", "*", "edge"), http.StatusOK},
	}
	for _, c := range cases {
		st := &trafficNotifyFakeStore{rows: map[string]*models.TrafficLimit{
			"user:u1|*|edge": {Scope: "user:u1", Region: "*", Kind: "edge", IncludedGB: &inc},
		}}
		rec := put(&TrafficLimitHandler{d: Deps{Store: st}}, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
		if c.want == http.StatusBadRequest && !strings.Contains(rec.Body.String(), storeOwnedTrafficRowMsg) {
			t.Errorf("%s: body %q does not say why", c.name, rec.Body.String())
		}
		if r := st.rows["user:u1|*|edge"]; r == nil || r.IncludedGB == nil || *r.IncludedGB != 5000 {
			t.Errorf("%s: the store's row changed: %+v", c.name, r)
		}
	}
}
