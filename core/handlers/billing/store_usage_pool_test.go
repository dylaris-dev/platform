package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// poolUsageStore answers GetUsage for one tenant with a managed row or not.
type poolUsageStore struct {
	store.Store
	managed *models.TrafficLimit
}

func (f *poolUsageStore) GetTrafficUsage(userID string, p time.Time) (*store.TrafficUsage, error) {
	return &store.TrafficUsage{UserID: userID, Period: p, EdgeBytes: 7, RelayBytes: 5}, nil
}
func (f *poolUsageStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return nil, nil
}
func (f *poolUsageStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if scope == "user:u1" && region == "*" && kind == "edge" {
		return f.managed, nil
	}
	return nil, nil
}
func (f *poolUsageStore) GetUserBilling(string) (*store.UserBilling, error)    { return nil, nil }
func (f *poolUsageStore) GetSetting(string) (string, error)                    { return "", nil }
func (f *poolUsageStore) BackupBytesByOwnerOnOwnStorage(string) (int64, error) { return 0, nil }

// The store bills a managed tenant's pool against the same "*" edge row the
// data throttle judges, so /store/usage hands it over; null for everyone else.
func TestStoreUsageManagedPool(t *testing.T) {
	inc, max := int64(5000), int64(10000)
	for _, c := range []struct {
		name    string
		managed *models.TrafficLimit
		want    string
	}{
		{"managed", &models.TrafficLimit{IncludedGB: &inc, MaxPurchaseGB: &max}, `{"includedGb":5000,"maxPurchaseGb":10000}`},
		{"byon", nil, `null`},
	} {
		h := NewStoreHandler(Deps{Store: &poolUsageStore{managed: c.managed}, StoreEnabled: true, StoreSharedKey: "k"})
		req := httptest.NewRequest(http.MethodGet, "/api/store/usage?uuid=u1", nil)
		req.Header.Set("X-Store-Key", "k")
		rec := httptest.NewRecorder()
		h.GetUsage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if string(out["managedPool"]) != c.want {
			t.Errorf("%s: managedPool %s, want %s", c.name, out["managedPool"], c.want)
		}
		if string(out["relayBytes"]) != "5" || string(out["edgeBytes"]) != "7" {
			t.Errorf("%s: totals %s / %s", c.name, out["edgeBytes"], out["relayBytes"])
		}
	}
}
