package billing

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

func provisionRaw(t *testing.T, body map[string]interface{}) (*storeLinkFakeStore, int) {
	t.Helper()
	fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
	rec := httptest.NewRecorder()
	body["uuid"], body["action"] = "u1", "activate"
	h.Provision(rec, storeLinkPost("/api/store/provision", body, storeLinkTestKey))
	return fs, rec.Code
}

func TestProvisionTrafficLimitsWritesUserRows(t *testing.T) {
	fs, code := provisionRaw(t, map[string]interface{}{"trafficLimits": []map[string]interface{}{
		{"region": "*", "kind": "edge", "includedGb": 100, "maxPurchaseGb": 0},
	}})
	if code != http.StatusOK || len(fs.trafficReplaces) != 1 {
		t.Fatalf("code=%d replaces=%+v", code, fs.trafficReplaces)
	}
	r := fs.trafficReplaces[0]
	if r.scope != "user:u1" || len(r.rows) != 1 {
		t.Fatalf("got %+v", r)
	}
	e := r.rows[0]
	if e.Region != "*" || e.Kind != "edge" || *e.IncludedGB != 100 || e.MaxPurchaseGB == nil || *e.MaxPurchaseGB != 0 {
		t.Fatalf("edge row %+v (0 must stay a number)", e)
	}
}

func TestProvisionTrafficLimitsEmptyDeletes(t *testing.T) {
	fs, code := provisionRaw(t, map[string]interface{}{"trafficLimits": []interface{}{}})
	if code != http.StatusOK || len(fs.trafficReplaces) != 1 || len(fs.trafficReplaces[0].rows) != 0 {
		t.Fatalf("code=%d replaces=%+v, want one replace with no rows", code, fs.trafficReplaces)
	}
	// null is "not set", never a wipe.
	fs, _ = provisionRaw(t, map[string]interface{}{"trafficLimits": nil})
	if len(fs.trafficReplaces) != 0 {
		t.Fatalf("null replaced: %+v", fs.trafficReplaces)
	}
}

func TestProvisionTrafficLimitsRefused(t *testing.T) {
	row := func(m map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"trafficLimits": []interface{}{m}}
	}
	for name, body := range map[string]map[string]interface{}{
		"negative included": row(map[string]interface{}{"region": "eu", "kind": "edge", "includedGb": -1}),
		"negative purchase": row(map[string]interface{}{"region": "eu", "kind": "edge", "maxPurchaseGb": -5}),
		"bad kind":          row(map[string]interface{}{"region": "eu", "kind": "Edge!"}),
		"bad region":        row(map[string]interface{}{"region": "EU", "kind": "edge"}),
		"no region":         row(map[string]interface{}{"kind": "edge"}),
		"fractional":        row(map[string]interface{}{"region": "eu", "kind": "edge", "includedGb": 1.5}),
		"duplicate": {"trafficLimits": []interface{}{
			map[string]interface{}{"region": "eu", "kind": "edge"}, map[string]interface{}{"region": "eu", "kind": "edge"}}},
		"not an array": {"trafficLimits": "x"},
		// Only ("*", edge) is store-owned: anything else is an operator's row.
		"per-region edge row":      row(map[string]interface{}{"region": "eu", "kind": "edge", "includedGb": 1}),
		"non-regional kind folded": row(map[string]interface{}{"region": "eu", "kind": "relay", "includedGb": 1}),
		"star relay row":           row(map[string]interface{}{"region": "*", "kind": "relay", "includedGb": 1}),
		"store row beside a per-region row": {"trafficLimits": []interface{}{
			map[string]interface{}{"region": "*", "kind": "edge"}, map[string]interface{}{"region": "eu", "kind": "edge"}}},
	} {
		body["maxRoutes"] = 3
		fs, code := provisionRaw(t, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400", name, code)
		}
		// Refused before the first write: nothing half-applied.
		if len(fs.trafficReplaces)+len(fs.routeLimitSets) != 0 {
			t.Errorf("%s: wrote before refusing", name)
		}
	}
}

func TestProvisionUnlimitedRoutesAndDomains(t *testing.T) {
	fs, code := provisionRaw(t, map[string]interface{}{"maxRoutes": "unlimited", "maxCustomDomains": "unlimited"})
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if len(fs.routeLimitSets) != 1 || fs.routeLimitSets[0].scope != "user:u1" || fs.routeLimitSets[0].max != nil || len(fs.routeLimitDeletes) != 0 {
		t.Fatalf("routes: sets=%+v dels=%+v, want one NULL upsert", fs.routeLimitSets, fs.routeLimitDeletes)
	}
	if len(fs.customDomainSets) != 1 || fs.customDomainSets[0].scope != "user:u1" || fs.customDomainSets[0].max != nil || len(fs.customDomainDels) != 0 {
		t.Fatalf("domains: sets=%+v dels=%+v, want one NULL upsert", fs.customDomainSets, fs.customDomainDels)
	}
	for _, f := range []string{"maxRoutes", "maxCustomDomains"} {
		fs, code := provisionRaw(t, map[string]interface{}{f: "lots"})
		if code != http.StatusBadRequest || len(fs.routeLimitSets)+len(fs.customDomainSets) != 0 {
			t.Errorf("%s=lots: code=%d sets=%+v", f, code, fs.routeLimitSets)
		}
	}
}

// An old store payload must produce exactly today's writes, and Provision must
// not reject unknown fields (an old Core ignores the new ones the same way).
func TestProvisionOldPayloadUntouched(t *testing.T) {
	fs, code := provisionRaw(t, map[string]interface{}{
		"maxNodes": 2, "maxRoutes": 12, "maxCustomDomains": nil, "somethingNew": []int{1},
	})
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if len(fs.trafficReplaces) != 0 {
		t.Fatalf("traffic touched: %+v", fs.trafficReplaces)
	}
	if len(fs.routeLimitSets) != 1 || *fs.routeLimitSets[0].max != 12 || len(fs.routeLimitDeletes) != 0 {
		t.Fatalf("routes sets=%+v dels=%+v", fs.routeLimitSets, fs.routeLimitDeletes)
	}
	if len(fs.customDomainSets) != 0 || len(fs.customDomainDels) != 1 {
		t.Fatalf("domains sets=%+v dels=%+v, want one delete", fs.customDomainSets, fs.customDomainDels)
	}
	if len(fs.entitlementCalls) != 1 {
		t.Fatalf("entitlement calls %+v", fs.entitlementCalls)
	}
}
