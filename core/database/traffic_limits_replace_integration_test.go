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
		_ = st.ReplaceTrafficLimits(scope, nil)
		_ = st.ReplaceTrafficLimits(other, nil)
	})

	if err := st.SetTrafficLimit(other, "*", "beam", gbPtr(1), gbPtr(2)); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceTrafficLimits(scope, []models.TrafficLimit{
		{Region: "eu", Kind: "edge", IncludedGB: gbPtr(100), MaxPurchaseGB: gbPtr(0)},
		{Region: "*", Kind: "beam", IncludedGB: nil, MaxPurchaseGB: gbPtr(5)},
	}); err != nil {
		t.Fatal(err)
	}
	// A different set: rows not in it are gone. Sent twice: idempotent.
	next := []models.TrafficLimit{{Region: "eu", Kind: "edge", IncludedGB: gbPtr(200), MaxPurchaseGB: nil}}
	for i := 0; i < 2; i++ {
		if err := st.ReplaceTrafficLimits(scope, next); err != nil {
			t.Fatal(err)
		}
	}
	if l, _ := st.GetTrafficLimit(scope, "*", "beam"); l != nil {
		t.Fatalf("stale row survived: %+v", l)
	}
	l, err := st.GetTrafficLimit(scope, "eu", "edge")
	if err != nil || l == nil || *l.IncludedGB != 200 || l.MaxPurchaseGB != nil {
		t.Fatalf("row = %+v err=%v, want included 200 and NULL purchase", l, err)
	}
	if o, _ := st.GetTrafficLimit(other, "*", "beam"); o == nil {
		t.Fatal("another scope was touched")
	}

	// A failing row rolls the whole replace back (NUL is refused in TEXT).
	if err := st.ReplaceTrafficLimits(scope, []models.TrafficLimit{
		{Region: "eu", Kind: "voice"}, {Region: "bad\x00", Kind: "edge"},
	}); err == nil {
		t.Fatal("bad row accepted")
	}
	if l, _ := st.GetTrafficLimit(scope, "eu", "edge"); l == nil || *l.IncludedGB != 200 {
		t.Fatalf("failed replace was not rolled back: %+v", l)
	}

	if err := st.ReplaceTrafficLimits(scope, []models.TrafficLimit{}); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.GetTrafficLimit(scope, "eu", "edge"); l != nil {
		t.Fatalf("empty set did not delete: %+v", l)
	}
}
