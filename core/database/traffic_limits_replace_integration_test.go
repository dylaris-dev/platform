package database

import (
	"testing"

	"dylaris-core/models"
)

func gbPtr(n int64) *int64 { return &n }

// Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationUserTrafficLimitRows(t *testing.T) {
	_, st := integrationDB(t)
	const scope, other = "user:replace-test", "user:replace-test-other"
	t.Cleanup(func() {
		for _, s := range []string{scope, other} {
			for _, rk := range [][2]string{{"*", "edge"}, {"eu", "edge"}, {"*", "relay"}} {
				_ = st.DeleteTrafficLimit(s, rk[0], rk[1])
			}
		}
	})

	// Operator rows: a per-region edge override and a relay row on the same
	// user, plus a store-owned row on another user.
	if err := st.SetTrafficLimit(scope, "eu", "edge", gbPtr(100), gbPtr(0)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTrafficLimit(scope, "*", "relay", gbPtr(10), nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTrafficLimit(other, "*", "edge", gbPtr(1), gbPtr(2)); err != nil {
		t.Fatal(err)
	}
	operatorRowsIntact := func(stage string) {
		t.Helper()
		if l, err := st.GetTrafficLimit(scope, "eu", "edge"); err != nil || l == nil || *l.IncludedGB != 100 || *l.MaxPurchaseGB != 0 {
			t.Fatalf("%s: per-region row = %+v err=%v", stage, l, err)
		}
		if l, err := st.GetTrafficLimit(scope, "*", "relay"); err != nil || l == nil || *l.IncludedGB != 10 || l.MaxPurchaseGB != nil {
			t.Fatalf("%s: relay row = %+v err=%v", stage, l, err)
		}
		if o, _ := st.GetTrafficLimit(other, "*", "edge"); o == nil {
			t.Fatalf("%s: another scope was touched", stage)
		}
	}

	// Managed activate, sent three times: idempotent, operator rows stay.
	pool := []models.TrafficLimit{{Region: "*", Kind: "edge", IncludedGB: gbPtr(5000), MaxPurchaseGB: nil}}
	for i := 0; i < 3; i++ {
		if err := st.ReplaceTrafficLimits(scope, pool); err != nil {
			t.Fatal(err)
		}
	}
	if l, err := st.GetTrafficLimit(scope, "*", "edge"); err != nil || l == nil || *l.IncludedGB != 5000 || l.MaxPurchaseGB != nil {
		t.Fatalf("pool row = %+v err=%v, want included 5000 and NULL purchase", l, err)
	}
	operatorRowsIntact("after activate")

	// Anything but the store-owned shape is refused before any write.
	if err := st.ReplaceTrafficLimits(scope, []models.TrafficLimit{{Region: "eu", Kind: "edge", IncludedGB: gbPtr(1)}}); err == nil {
		t.Fatal("per-region row accepted")
	}
	if l, _ := st.GetTrafficLimit(scope, "*", "edge"); l == nil || *l.IncludedGB != 5000 {
		t.Fatalf("refused replace changed the pool row: %+v", l)
	}
	operatorRowsIntact("after a refused replace")

	// A failing insert (the second row breaks the unique key) rolls the delete
	// back.
	bad := []models.TrafficLimit{{Region: "*", Kind: "edge", IncludedGB: gbPtr(1)}, {Region: "*", Kind: "edge", IncludedGB: gbPtr(2)}}
	if err := st.ReplaceTrafficLimits(scope, bad); err == nil {
		t.Fatal("duplicate row accepted")
	}
	if l, _ := st.GetTrafficLimit(scope, "*", "edge"); l == nil || *l.IncludedGB != 5000 {
		t.Fatalf("failed replace was not rolled back: %+v", l)
	}

	// The wipe: only the pool row goes.
	if err := st.ReplaceTrafficLimits(scope, []models.TrafficLimit{}); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.GetTrafficLimit(scope, "*", "edge"); l != nil {
		t.Fatalf("empty set did not delete the pool row: %+v", l)
	}
	operatorRowsIntact("after the wipe")
}
