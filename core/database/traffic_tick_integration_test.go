package database

import (
	"testing"
	"time"

	"dylaris-core/store"
)

// One tenant's tick lands whole or not at all: overage bills off the region
// cells, so a total written without its cells is traffic never billed.
// Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationATrafficTickIsOneTransaction(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	period := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// A NUL byte is refused by Postgres in TEXT: the second cell fails after
	// the total and the first cell were written inside the transaction.
	err := st.AddTrafficTick(f.user.ID, period, 500, 0, []store.TrafficTickCell{
		{Region: "eu-central", Kind: "edge", Product: "byon", Bytes: 300, Voice: 50},
		{Region: "bad\x00", Kind: "edge", Product: "byon", Bytes: 200},
	})
	if err == nil {
		t.Fatal("the bad cell was accepted")
	}
	u, _ := st.GetTrafficUsage(f.user.ID, period)
	cells, _ := st.GetTrafficUsageRegions(f.user.ID, period)
	if (u != nil && u.EdgeBytes != 0) || len(cells) != 0 {
		t.Fatalf("a failed tick left total %+v and cells %+v", u, cells)
	}

	if err := st.AddTrafficTick(f.user.ID, period, 500, 0, []store.TrafficTickCell{
		{Region: "eu-central", Kind: "edge", Product: "byon", Bytes: 500, Voice: 50},
	}); err != nil {
		t.Fatal(err)
	}
	u, _ = st.GetTrafficUsage(f.user.ID, period)
	cells, _ = st.GetTrafficUsageRegions(f.user.ID, period)
	if u == nil || u.EdgeBytes != 500 || len(cells) != 1 || cells[0].Bytes != 500 || cells[0].VoiceBytes != 50 {
		t.Fatalf("total %+v cells %+v, want 500 with one 500-byte cell, 50 of it voice", u, cells)
	}
}
