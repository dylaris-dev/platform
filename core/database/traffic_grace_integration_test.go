package database

import (
	"testing"
	"time"

	"dylaris-core/store"
)

// The grace spell is opened and closed by single UPDATEs that read the managed
// relay bytes (relay cells of product route) in the statement itself, so the
// snapshot is the row's value at that moment, a second open or close is a
// no-op, and the tenant's server relay bytes (product byon) never move it.
// Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationTrafficGraceSpell(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	period := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tick := func(managed, server int64) {
		t.Helper()
		if err := st.AddTrafficTick(f.user.ID, period, 0, managed+server, []store.TrafficTickCell{
			{Region: "eu-central", Kind: store.TrafficKindRelay, Product: store.TrafficProductRoute, Bytes: managed},
			{Region: "eu-central", Kind: store.TrafficKindRelay, Product: store.TrafficProductBYON, Bytes: server},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if ok, err := st.StartTrafficGrace(f.user.ID, period); err != nil || ok {
		t.Fatalf("start without a row: %v %v, want a no-op", ok, err)
	}
	tick(40, 7)
	if ok, err := st.StartTrafficGrace(f.user.ID, period); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if ok, _ := st.StartTrafficGrace(f.user.ID, period); ok {
		t.Fatal("a second start moved the snapshot")
	}
	tick(500, 300)
	u, err := st.GetTrafficUsage(f.user.ID, period)
	if err != nil || u.GraceSinceRelay == nil || *u.GraceSinceRelay != 40 || u.ManagedRelayBytes != 540 ||
		u.GraceRelayBytes() != 500 || u.RelayBytes != 847 {
		t.Fatalf("open spell: %+v %v", u, err)
	}
	if ok, err := st.EndTrafficGrace(f.user.ID, period); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	if ok, _ := st.EndTrafficGrace(f.user.ID, period); ok {
		t.Fatal("a second end counted the spell twice")
	}
	tick(30, 0)
	u, _ = st.GetTrafficUsage(f.user.ID, period)
	if u.GraceSinceRelay != nil || u.GraceExcludedBytes != 500 || u.GraceRelayBytes() != 500 || u.ManagedRelayBytes != 570 {
		t.Fatalf("closed spell: %+v", u)
	}
}
