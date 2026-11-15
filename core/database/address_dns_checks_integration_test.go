package database

import (
	"strings"
	"testing"
	"time"
)

func dnsCheckName() string {
	return uniqueName("dns_") + ".example.test"
}

// Phase 70 on a database without the table creates it and its index, and a
// second boot is a no-op that keeps the rows. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationAddressDNSChecksSchema(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DROP TABLE IF EXISTS address_dns_checks`); err != nil {
		t.Fatal(err)
	}
	if err := applyAddressDNSChecks(db); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	name := dnsCheckName()
	if _, err := st.RecordAddressDNSCheck(name, "ok", "eu", true); err != nil {
		t.Fatal(err)
	}
	if err := applyAddressDNSChecks(db); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	got, err := st.GetAddressDNSChecks([]string{name})
	if err != nil || got[name].Status != "ok" || got[name].Region != "eu" {
		t.Fatalf("after second boot: %+v, %v", got, err)
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'idx_address_dns_checks_checked_at'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("index count %d, err %v", idx, err)
	}
}

// miss, miss, ok: misses 1, 2, 0; failing_since is set by the first miss, kept
// by the second and cleared by the ok. The name is stored lower case.
func TestIntegrationRecordAddressDNSCheckRuns(t *testing.T) {
	db, st := integrationDB(t)
	name := "play." + dnsCheckName()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM address_dns_checks WHERE name = $1`, name) })

	first, err := st.RecordAddressDNSCheck(strings.ToUpper(name), "elsewhere", "", false)
	if err != nil || first.Name != name || first.Misses != 1 || first.FailingSince == nil {
		t.Fatalf("first miss = %+v, %v", first, err)
	}
	if _, err := st.RecordAddressDNSCheck(name, "partial", "", false); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetAddressDNSChecks([]string{name})
	if err != nil {
		t.Fatal(err)
	}
	row := got[name]
	if row.Misses != 2 || row.Status != "partial" || row.FailingSince == nil || !row.FailingSince.Equal(*first.FailingSince) {
		t.Fatalf("second miss = %+v; want misses 2 and the first miss's failing_since %v", row, first.FailingSince)
	}
	ok, err := st.RecordAddressDNSCheck(name, "ok", "eu", true)
	if err != nil || ok.Misses != 0 || ok.FailingSince != nil || ok.Status != "ok" || ok.Region != "eu" {
		t.Fatalf("ok = %+v, %v; want misses 0 and no failing_since", ok, err)
	}
	if ok.CheckedAt.Before(row.CheckedAt) {
		t.Fatalf("checked_at went back: %v before %v", ok.CheckedAt, row.CheckedAt)
	}
	again, err := st.RecordAddressDNSCheck(name, "ipv6", "", false)
	if err != nil || again.Misses != 1 || again.FailingSince == nil {
		t.Fatalf("a miss after an ok = %+v, %v; want a fresh run", again, err)
	}
}

// "Could not check" is never stored: lookup_failed, rate_limited, an empty or
// made-up status are errors and leave no row.
func TestIntegrationRecordRefusesUnknown(t *testing.T) {
	db, st := integrationDB(t)
	name := dnsCheckName()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM address_dns_checks WHERE name = $1`, name) })
	for _, status := range []string{"lookup_failed", "rate_limited", "", "unknown"} {
		if _, err := st.RecordAddressDNSCheck(name, status, "", false); err == nil {
			t.Fatalf("%q was stored", status)
		}
	}
	got, err := st.GetAddressDNSChecks([]string{name})
	if err != nil || len(got) != 0 {
		t.Fatalf("rows %+v, %v; want none", got, err)
	}
}

// Rows whose name is in the list stay, the rest go; due lists the old ones.
func TestIntegrationDeleteAddressDNSChecksNotIn(t *testing.T) {
	db, st := integrationDB(t)
	keep, drop := dnsCheckName(), dnsCheckName()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM address_dns_checks WHERE name IN ($1, $2)`, keep, drop) })
	for _, n := range []string{keep, drop} {
		if _, err := st.RecordAddressDNSCheck(n, "ok", "eu", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE address_dns_checks SET checked_at = NOW() - INTERVAL '2 days' WHERE name = $1`, keep); err != nil {
		t.Fatal(err)
	}
	due, err := st.ListAddressDNSChecksDue(time.Now().Add(-24*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var sawKeep, sawDrop bool
	for _, c := range due {
		sawKeep = sawKeep || c.Name == keep
		sawDrop = sawDrop || c.Name == drop
	}
	if !sawKeep || sawDrop {
		t.Fatalf("due %+v; want %s and not %s", due, keep, drop)
	}

	// Other tests' rows share the table: keep them too, so only drop goes.
	var others []string
	rows, err := db.Query(`SELECT name FROM address_dns_checks WHERE name <> $1`, drop)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		others = append(others, n)
	}
	rows.Close()
	n, err := st.DeleteAddressDNSChecksNotIn(others)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v; want 1", n, err)
	}
	got, err := st.GetAddressDNSChecks([]string{keep, drop})
	if err != nil || len(got) != 1 || got[keep].Name != keep {
		t.Fatalf("left %+v, %v; want only %s", got, err, keep)
	}
}
