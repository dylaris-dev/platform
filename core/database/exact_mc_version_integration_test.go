package database

import "testing"

// The SQL mapping must answer exactly what handlers.neoForgeMinecraft does;
// these are the cases of TestNeoForgeMinecraft.
func TestIntegrationNeoForgeMinecraftSQL(t *testing.T) {
	db, _ := integrationDB(t)
	for _, tt := range []struct{ loader, want string }{
		{"21.1.264", "1.21.1"},
		{"21.0.5", "1.21"},
		{"21.10.3", "1.21.10"},
		{"20.4.237", "1.20.4"},
		{"20.2.86", "1.20.2"},
		{"26.1.2.5", "26.1.2"},
		{"26.2.0.7", "26.2"},
		{"26.3.0.89", "26.3"},
		{"21.1.0-beta", ""},
		{"26.1.0.0-alpha.4+snapshot-1", ""},
		{"0.25w14craftmine.3", ""},
		{"0.1.2", ""},
		{"21.1", ""},
		{"", ""},
		{"garbage", ""},
		{"99999999999.1.2", ""},
	} {
		var got string
		if err := db.QueryRow(`SELECT `+neoForgeMinecraftSQL+` FROM (SELECT $1::text AS l) v`, tt.loader).Scan(&got); err != nil {
			t.Fatalf("%q: %v", tt.loader, err)
		}
		if got != tt.want {
			t.Errorf("SQL mapping of %q = %q, want %q", tt.loader, got, tt.want)
		}
	}
}

// The backfill fixes exactly the rows it can prove, and a second run changes
// nothing.
func TestIntegrationExactMcVersionBackfill(t *testing.T) {
	db, st := integrationDB(t)
	neo := newFixture(t, st)
	forge := newFixture(t, st)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE servers SET installer_type='neoforge', minecraft_version='', build_number='', active_sub_server='a' WHERE id=$1`, neo.server.ID)
	exec(`UPDATE servers SET installer_type='forge', minecraft_version='1.21', build_number='1.21.1', active_sub_server='x' WHERE id=$1`, forge.server.ID)
	ins := `INSERT INTO sub_server_installs (server_id, sub_server_name, installer_type, mc_version, build_version, loader) VALUES ($1,$2,$3,$4,$5,$6)`
	exec(ins, neo.server.ID, "a", "neoforge", "", "", "21.1.264")         // -> 1.21.1
	exec(ins, neo.server.ID, "b", "neoforge", "1.21", "", "21.1.264")     // line -> 1.21.1
	exec(ins, neo.server.ID, "c", "neoforge", "", "", "21.1.0-beta")      // untouched
	exec(ins, neo.server.ID, "d", "neoforge", "1.20.4", "", "21.1.264")   // conflict, untouched
	exec(ins, forge.server.ID, "x", "forge", "1.21", "1.21.1", "")        // -> 1.21.1
	exec(ins, forge.server.ID, "y", "fabric", "1.21.4", "0.16.9", "")     // loader build, untouched
	exec(ins, forge.server.ID, "z", "forge", "1.20", "1.20.1-47.3.0", "") // forge build, untouched
	exec(ins, forge.server.ID, "w", "paper", "", "1.21.1", "")            // empty mc, untouched

	for run := 1; run <= 2; run++ {
		if err := applyExactMcVersionBackfill(db); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		for _, c := range []struct {
			id   int
			sub  string
			want string
		}{
			{neo.server.ID, "a", "1.21.1"},
			{neo.server.ID, "b", "1.21.1"},
			{neo.server.ID, "c", ""},
			{neo.server.ID, "d", "1.20.4"},
			{forge.server.ID, "x", "1.21.1"},
			{forge.server.ID, "y", "1.21.4"},
			{forge.server.ID, "z", "1.20"},
			{forge.server.ID, "w", ""},
		} {
			var got string
			if err := db.QueryRow(`SELECT mc_version FROM sub_server_installs WHERE server_id=$1 AND sub_server_name=$2`, c.id, c.sub).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("run %d: install %s mc_version = %q, want %q", run, c.sub, got, c.want)
			}
		}
		for _, c := range []struct {
			id                int
			wantMC, wantBuild string
		}{{neo.server.ID, "1.21.1", "21.1.264"}, {forge.server.ID, "1.21.1", "1.21.1"}} {
			var mc, build string
			if err := db.QueryRow(`SELECT minecraft_version, build_number FROM servers WHERE id=$1`, c.id).Scan(&mc, &build); err != nil {
				t.Fatal(err)
			}
			if mc != c.wantMC || build != c.wantBuild {
				t.Errorf("run %d: server %d = %q/%q, want %q/%q", run, c.id, mc, build, c.wantMC, c.wantBuild)
			}
		}
		for _, c := range []struct{ sub, want string }{{"a", "21.1.264"}, {"c", "21.1.0-beta"}, {"x", "1.21.1"}, {"y", "0.16.9"}} {
			id := neo.server.ID
			if c.sub == "x" || c.sub == "y" {
				id = forge.server.ID
			}
			var got string
			if err := db.QueryRow(`SELECT build_version FROM sub_server_installs WHERE server_id=$1 AND sub_server_name=$2`, id, c.sub).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("run %d: install %s build_version = %q, want %q", run, c.sub, got, c.want)
			}
		}
	}
}
