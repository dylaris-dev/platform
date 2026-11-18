package billing

import (
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// managedPoolStore is a managed-gateway tenant: a user "*" edge row holding the
// ACCOUNT total, plus user_default rows for everyone else.
type managedPoolStore struct {
	store.Store
	managed *int64 // the user "*" edge row's included GB; nil: not managed
	cells   []store.RegionUsage
	usage   store.TrafficUsage
}

func (f *managedPoolStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return f.cells, nil
}
func (f *managedPoolStore) GetTrafficUsage(string, time.Time) (*store.TrafficUsage, error) {
	u := f.usage
	return &u, nil
}
func (f *managedPoolStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if scope != services.TrafficScopeDefault {
		if f.managed != nil && region == services.TrafficRegionAny && kind == store.TrafficKindEdge {
			return &models.TrafficLimit{IncludedGB: f.managed}, nil
		}
		return nil, nil
	}
	gb := int64(100)
	return &models.TrafficLimit{IncludedGB: &gb}, nil
}
func (f *managedPoolStore) GetUserDataBillingEnabled(string) (bool, error) { return false, nil }
func (f *managedPoolStore) GetSetting(string) (string, error)              { return "", nil }

// The managed pool is one pool for the account, as JudgeManagedPool judges it:
// never once per region, never multiplied by the units, and the own-ticket
// relay bytes are in it rather than in the BYON data pool.
func TestTrafficStatusShowsTheManagedPoolOnce(t *testing.T) {
	gb := int64(trafficGB)
	pool := int64(15000)
	three := int64(3)
	cells := []store.RegionUsage{
		{Region: "eu", Kind: store.TrafficKindEdge, Product: store.TrafficProductRoute, Bytes: 6000 * gb},
		{Region: "us", Kind: store.TrafficKindEdge, Product: store.TrafficProductRoute, Bytes: 6000 * gb},
		{Region: "*", Kind: store.TrafficKindRelay, Product: store.TrafficProductRoute, Bytes: 1000 * gb},
		{Region: "*", Kind: store.TrafficKindRelay, Product: store.TrafficProductBYON, Bytes: 50 * gb},
	}
	cases := []struct {
		name      string
		managed   *int64
		wantEdges map[string]int64 // region -> included GB
		wantUsed  map[string]int64 // region -> used GB
		wantRelay int64            // used GB in the data pool
		wantWarn  int
	}{
		{"managed tenant", &pool, map[string]int64{"*": 15000}, map[string]int64{"*": 13000}, 50, 80},
		// No managed row: per-region pools times units, and still no own-ticket
		// relay bytes in the data pool.
		{"not managed", nil, map[string]int64{"eu": 300, "us": 300}, map[string]int64{"eu": 6000, "us": 6000}, 50, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &managedPoolStore{managed: tc.managed, cells: cells,
				usage: store.TrafficUsage{EdgeBytes: 12000 * gb, RelayBytes: 1050 * gb, ManagedRelayBytes: 1000 * gb}}
			h := &BillingHandler{d: Deps{Store: fs}}
			st := h.trafficStatusFor("u1", &store.UserBilling{MaxLinks: &three})
			if st == nil {
				t.Fatal("no traffic status")
			}
			edges := map[string]int64{}
			for _, p := range st.Pools {
				switch p.Kind {
				case store.TrafficKindEdge:
					if p.IncludedGB == nil {
						t.Fatalf("edge pool %s has no allowance", p.Region)
					}
					edges[p.Region] = *p.IncludedGB
					if p.UsedGB != tc.wantUsed[p.Region] {
						t.Errorf("edge %s used %d GB, want %d", p.Region, p.UsedGB, tc.wantUsed[p.Region])
					}
				case store.TrafficKindRelay:
					if p.UsedGB != tc.wantRelay {
						t.Errorf("data pool used %d GB, want %d - own-ticket relay bytes are not BYON data", p.UsedGB, tc.wantRelay)
					}
				}
			}
			if len(edges) != len(tc.wantEdges) {
				t.Fatalf("edge pools %v, want %v", edges, tc.wantEdges)
			}
			for r, want := range tc.wantEdges {
				if edges[r] != want {
					t.Errorf("edge pool %s included %d GB, want %d", r, edges[r], want)
				}
			}
			if st.Warn != tc.wantWarn {
				t.Errorf("warn %d, want %d", st.Warn, tc.wantWarn)
			}
		})
	}
}
