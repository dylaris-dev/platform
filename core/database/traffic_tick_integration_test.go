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
	marker := "dylaris:traffic:agg:seen:edge:" + f.user.ID + "-tick-test"
	t.Cleanup(func() { st.PruneTrafficSeen(time.Now().Add(time.Hour)) })

	// A NUL byte is refused by Postgres in TEXT: the second cell fails after
	// the total and the first cell were written inside the transaction.
	err := st.AddTrafficTick(f.user.ID, period, 500, 0, []store.TrafficTickCell{
		{Region: "eu-central", Kind: "edge", Product: "byon", Bytes: 300, Voice: 50},
		{Region: "bad\x00", Kind: "edge", Product: "byon", Bytes: 200},
	}, []store.TrafficSeen{{Key: marker, Val: 500}})
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
	}, []store.TrafficSeen{{Key: marker, Val: 500}}); err != nil {
		t.Fatal(err)
	}
	// The marker is the other half of the same promise: it lands with the
	// usage it stands for, and only then.
	if seen, err := st.LoadTrafficSeen(); err != nil || seen[marker] != 500 {
		t.Fatalf("marker after the tick that landed = %d (%v), want 500", seen[marker], err)
	}
	u, _ = st.GetTrafficUsage(f.user.ID, period)
	cells, _ = st.GetTrafficUsageRegions(f.user.ID, period)
	if u == nil || u.EdgeBytes != 500 || len(cells) != 1 || cells[0].Bytes != 500 || cells[0].VoiceBytes != 50 {
		t.Fatalf("total %+v cells %+v, want 500 with one 500-byte cell, 50 of it voice", u, cells)
	}
}

// A tick that rolled back must not leave its marker behind: a marker without
// its usage is traffic that is never billed, the mirror image of the double
// count the table exists to prevent.
func TestIntegrationARolledBackTickLeavesNoMarker(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	period := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	marker := "dylaris:traffic:agg:seen:edge:" + f.user.ID + "-rollback-test"
	t.Cleanup(func() { st.PruneTrafficSeen(time.Now().Add(time.Hour)) })

	if err := st.SetTrafficSeen(marker, 100); err != nil {
		t.Fatal(err)
	}
	err := st.AddTrafficTick(f.user.ID, period, 500, 0, []store.TrafficTickCell{
		{Region: "bad\x00", Kind: "edge", Product: "byon", Bytes: 500},
	}, []store.TrafficSeen{{Key: marker, Val: 600}})
	if err == nil {
		t.Fatal("the bad cell was accepted")
	}
	seen, err := st.LoadTrafficSeen()
	if err != nil || seen[marker] != 100 {
		t.Fatalf("marker after a rolled-back tick = %d (%v), want the old 100", seen[marker], err)
	}

	// Pruning removes what nothing has written since the cut, and only that.
	if n, err := st.PruneTrafficSeen(time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("pruned %d fresh markers (%v), want 0", n, err)
	}
	if _, err := st.PruneTrafficSeen(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if seen, _ := st.LoadTrafficSeen(); len(seen) != 0 {
		t.Fatalf("markers left after pruning everything: %v", seen)
	}
}
