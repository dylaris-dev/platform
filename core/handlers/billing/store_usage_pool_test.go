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
	usage   *store.TrafficUsage
	cells   []store.RegionUsage
}

func (f *poolUsageStore) GetTrafficUsage(userID string, p time.Time) (*store.TrafficUsage, error) {
	if f.usage != nil {
		return f.usage, nil
	}
	return &store.TrafficUsage{UserID: userID, Period: p, EdgeBytes: 7, RelayBytes: 5}, nil
}
func (f *poolUsageStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return f.cells, nil
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
		{"managed", &models.TrafficLimit{IncludedGB: &inc, MaxPurchaseGB: &max}, `{"includedGb":5000,"maxPurchaseGb":10000,"relayBytes":0}`},
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

// Data grace bytes leave relayBytes, which the store sums into the pool it
// bills, cuts off on and takes the consent baseline from, and are reported on
// their own. edgeBytes and the region breakdown keep the full values.
func TestStoreUsageReportsGraceApart(t *testing.T) {
	inc := int64(100)
	since, cap90 := int64(40), int64(90)
	for _, c := range []struct {
		name               string
		usage              store.TrafficUsage
		wantRelay, wantGrc string
	}{
		{"no grace", store.TrafficUsage{EdgeBytes: 60, RelayBytes: 40, ManagedRelayBytes: 40}, "40", "0"},
		{"open spell", store.TrafficUsage{EdgeBytes: 60, RelayBytes: 540, ManagedRelayBytes: 540, GraceSinceRelay: &since}, "40", "500"},
		{"closed spell then billable", store.TrafficUsage{EdgeBytes: 60, RelayBytes: 570, ManagedRelayBytes: 570, GraceExcludedBytes: 500}, "70", "500"},
		// A consent tenant's spell at the store cap is stored the same way.
		{"consent spell at the store cap", store.TrafficUsage{EdgeBytes: 60, RelayBytes: 490, ManagedRelayBytes: 490, GraceSinceRelay: &cap90}, "90", "400"},
	} {
		u := c.usage
		h := NewStoreHandler(Deps{Store: &poolUsageStore{managed: &models.TrafficLimit{IncludedGB: &inc}, usage: &u}, StoreEnabled: true, StoreSharedKey: "k"})
		req := httptest.NewRequest(http.MethodGet, "/api/store/usage?uuid=u1", nil)
		req.Header.Set("X-Store-Key", "k")
		rec := httptest.NewRecorder()
		h.GetUsage(rec, req)
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(out["relayBytes"]) != c.wantRelay || string(out["graceRelayBytes"]) != c.wantGrc || string(out["edgeBytes"]) != "60" {
			t.Errorf("%s: relay %s grace %s edge %s, want %s %s 60", c.name, out["relayBytes"], out["graceRelayBytes"], out["edgeBytes"], c.wantRelay, c.wantGrc)
		}
		// managedPool.relayBytes is what the website bills and cuts off by.
		var pool struct {
			RelayBytes json.RawMessage `json:"relayBytes"`
		}
		if err := json.Unmarshal(out["managedPool"], &pool); err != nil || string(pool.RelayBytes) != c.wantRelay {
			t.Errorf("%s: managedPool.relayBytes %s (%v), want %s without grace bytes", c.name, pool.RelayBytes, err, c.wantRelay)
		}
	}
}

// Own-ticket relay bytes (product route) are the managed pool's: reported in
// managedPool.relayBytes without grace, and kept out of the region breakdown
// the BYON data overage bills from, managed or not (a cancelled plan).
func TestStoreUsageKeepsManagedRelayOutOfTheBreakdown(t *testing.T) {
	inc := int64(100)
	since := int64(40)
	cells := []store.RegionUsage{
		{Region: "eu-central", Kind: "relay", Product: store.TrafficProductRoute, Bytes: 540},
		{Region: "eu-central", Kind: "relay", Product: store.TrafficProductBYON, Bytes: 70},
		{Region: "eu-central", Kind: "edge", Product: store.TrafficProductBYON, Bytes: 60},
	}
	usage := store.TrafficUsage{EdgeBytes: 60, RelayBytes: 610, ManagedRelayBytes: 540, GraceSinceRelay: &since}
	for _, c := range []struct {
		name    string
		managed *models.TrafficLimit
		pool    string
	}{
		{"managed", &models.TrafficLimit{IncludedGB: &inc}, `{"includedGb":100,"maxPurchaseGb":null,"relayBytes":40}`},
		{"cancelled", nil, `null`},
	} {
		u := usage
		h := NewStoreHandler(Deps{Store: &poolUsageStore{managed: c.managed, usage: &u, cells: cells}, StoreEnabled: true, StoreSharedKey: "k"})
		req := httptest.NewRequest(http.MethodGet, "/api/store/usage?uuid=u1", nil)
		req.Header.Set("X-Store-Key", "k")
		rec := httptest.NewRecorder()
		h.GetUsage(rec, req)
		var out struct {
			ManagedPool json.RawMessage `json:"managedPool"`
			Regions     []struct {
				Kind  string `json:"kind"`
				Bytes int64  `json:"bytes"`
			} `json:"regions"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v %s", c.name, err, rec.Body.String())
		}
		if string(out.ManagedPool) != c.pool {
			t.Errorf("%s: managedPool %s, want %s", c.name, out.ManagedPool, c.pool)
		}
		for _, r := range out.Regions {
			if r.Kind == "relay" && r.Bytes != 70 {
				t.Errorf("%s: relay breakdown %d bytes, want only the 70 server bytes", c.name, r.Bytes)
			}
		}
	}
}
