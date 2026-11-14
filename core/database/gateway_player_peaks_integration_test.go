package database

import (
	"strings"
	"testing"
	"time"
)

func peakOwner(t *testing.T) string {
	return "peak-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + time.Now().Format("150405.000000000")
}

// Phase 68 on a database without the table creates it and its index, and a
// second boot is a no-op that keeps the rows. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationPlayerPeaksSchema(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DROP TABLE IF EXISTS gateway_player_peaks`); err != nil {
		t.Fatal(err)
	}
	if err := applyGatewayPlayerPeaks(db); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	owner := peakOwner(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM gateway_player_peaks WHERE owner_id = $1`, owner) })
	day := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	if err := st.RaisePlayerPeak(owner, day, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := applyGatewayPlayerPeaks(db); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	rows, err := st.ListPlayerPeaks(owner, day)
	if err != nil || len(rows) != 1 || rows[0].Total != 3 {
		t.Fatalf("after second boot: %+v, %v", rows, err)
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'idx_gateway_player_peaks_day'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("index count %d, err %v", idx, err)
	}
}

// Each column is its own maximum; a lower sample leaves the row; another day
// is another row; the list is newest first and starts at since.
func TestIntegrationRaisePlayerPeakOnlyRaises(t *testing.T) {
	db, st := integrationDB(t)
	owner, other := peakOwner(t), peakOwner(t)+"-other"
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM gateway_player_peaks WHERE owner_id IN ($1, $2)`, owner, other) })
	d1 := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	steps := []struct{ j, b, tot int }{{5, 1, 6}, {2, 0, 2}, {1, 4, 5}, {4, 3, 7}}
	for _, s := range steps {
		if err := st.RaisePlayerPeak(owner, d2, s.j, s.b, s.tot); err != nil {
			t.Fatal(err)
		}
	}
	// A time late in the day in another zone still names its own date.
	late := time.Date(2026, 11, 13, 23, 59, 59, 0, time.FixedZone("X", -5*3600))
	if err := st.RaisePlayerPeak(owner, time.Date(late.Year(), late.Month(), late.Day(), 0, 0, 0, 0, time.UTC), 9, 0, 9); err != nil {
		t.Fatal(err)
	}
	if err := st.RaisePlayerPeak(other, d2, 50, 50, 100); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListPlayerPeaks(owner, d1)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v, %v", rows, err)
	}
	if !rows[0].Day.Equal(d2) || rows[0].Java != 5 || rows[0].Bedrock != 4 || rows[0].Total != 7 {
		t.Errorf("newest = %+v, want 2026-11-14 5/4/7", rows[0])
	}
	if !rows[1].Day.Equal(d1) || rows[1].Java != 9 || rows[1].Total != 9 {
		t.Errorf("older = %+v, want 2026-11-13 9/0/9", rows[1])
	}
	if rows, _ := st.ListPlayerPeaks(owner, d2); len(rows) != 1 {
		t.Errorf("since %s: %+v", d2, rows)
	}
}

// The purge removes rows before the cut only, never today's whatever cut it is
// given, and a second run removes nothing more.
func TestIntegrationPurgePlayerPeaks(t *testing.T) {
	db, st := integrationDB(t)
	owner := peakOwner(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM gateway_player_peaks WHERE owner_id = $1`, owner) })
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, d := range []time.Time{today.AddDate(0, 0, -500), today.AddDate(0, 0, -400), today.AddDate(0, 0, -1), today} {
		if err := st.RaisePlayerPeak(owner, d, 1, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		rows, err := st.ListPlayerPeaks(owner, today.AddDate(0, 0, -1000))
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if _, err := st.PurgePlayerPeaks(today.AddDate(0, 0, -400)); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Fatalf("after the retention purge: %d rows, want 3 (day -400 kept)", n)
	}
	if _, err := st.PurgePlayerPeaks(today.AddDate(0, 0, -400)); err != nil || count() != 3 {
		t.Fatalf("second run: %d rows, %v", count(), err)
	}
	// A cut in the future cannot reach today's row.
	if _, err := st.PurgePlayerPeaks(today.AddDate(0, 0, 5)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListPlayerPeaks(owner, today.AddDate(0, 0, -1000))
	if len(rows) != 1 || !rows[0].Day.Equal(today) {
		t.Fatalf("after a future cut: %+v, want today's row only", rows)
	}
}
