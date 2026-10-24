package database

import "testing"

// A server runs in its own zone, else its owner's, else UTC. The inheritance
// is SQL (GetServerTimezone and GetServerByID), so it is pinned against a real
// Postgres. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationAServerInheritsItsOwnersZone(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	check := func(step, wantOwn, wantEff, wantSrv string) {
		t.Helper()
		own, eff, err := st.GetServerTimezone(f.server.ID)
		if err != nil {
			t.Fatalf("%s: GetServerTimezone: %v", step, err)
		}
		srv, err := st.GetServerByID(f.server.ID)
		if err != nil {
			t.Fatalf("%s: GetServerByID: %v", step, err)
		}
		if own != wantOwn || eff != wantEff || srv.Timezone != wantSrv {
			t.Fatalf("%s: own=%q effective=%q srv.Timezone=%q, want %q %q %q", step, own, eff, srv.Timezone, wantOwn, wantEff, wantSrv)
		}
	}

	check("nothing chosen", "", "UTC", "UTC")
	if err := st.SetUserTimePreferences(f.user.ID, "Europe/Berlin", "12h"); err != nil {
		t.Fatalf("SetUserTimePreferences: %v", err)
	}
	if tz, format, err := st.GetUserTimePreferences(f.user.ID); err != nil || tz != "Europe/Berlin" || format != "12h" {
		t.Fatalf("read back %q %q %v", tz, format, err)
	}
	check("owner chose", "", "Europe/Berlin", "Europe/Berlin")
	if err := st.SetServerTimezone(f.server.ID, "America/New_York"); err != nil {
		t.Fatalf("SetServerTimezone: %v", err)
	}
	check("server overrides", "America/New_York", "America/New_York", "America/New_York")
	if err := st.SetServerTimezone(f.server.ID, ""); err != nil {
		t.Fatalf("SetServerTimezone clear: %v", err)
	}
	check("server inherits again", "", "Europe/Berlin", "Europe/Berlin")
	// Clearing every choice runs the container in UTC again; an empty zone
	// would tell the node to keep the one it has.
	if err := st.SetUserTimePreferences(f.user.ID, "", "auto"); err != nil {
		t.Fatal(err)
	}
	check("nothing chosen any more", "", "UTC", "UTC")
}
