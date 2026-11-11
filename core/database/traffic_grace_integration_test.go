package database

import (
	"testing"
	"time"
)

// The grace spell is opened and closed by single UPDATEs that read relay_bytes
// in the statement itself, so the snapshot is the row's value at that moment
// and a second open or close is a no-op. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationTrafficGraceSpell(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	period := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	if ok, err := st.StartTrafficGrace(f.user.ID, period); err != nil || ok {
		t.Fatalf("start without a row: %v %v, want a no-op", ok, err)
	}
	if err := st.AddTrafficUsage(f.user.ID, period, 60, 40); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.StartTrafficGrace(f.user.ID, period); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if ok, _ := st.StartTrafficGrace(f.user.ID, period); ok {
		t.Fatal("a second start moved the snapshot")
	}
	if err := st.AddTrafficUsage(f.user.ID, period, 0, 500); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetTrafficUsage(f.user.ID, period)
	if err != nil || u.GraceSinceRelay == nil || *u.GraceSinceRelay != 40 || u.GraceRelayBytes() != 500 || u.RelayBytes != 540 {
		t.Fatalf("open spell: %+v %v", u, err)
	}
	if ok, err := st.EndTrafficGrace(f.user.ID, period); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	if ok, _ := st.EndTrafficGrace(f.user.ID, period); ok {
		t.Fatal("a second end counted the spell twice")
	}
	if err := st.AddTrafficUsage(f.user.ID, period, 0, 30); err != nil {
		t.Fatal(err)
	}
	u, _ = st.GetTrafficUsage(f.user.ID, period)
	if u.GraceSinceRelay != nil || u.GraceExcludedBytes != 500 || u.GraceRelayBytes() != 500 || u.RelayBytes != 570 {
		t.Fatalf("closed spell: %+v", u)
	}
}
