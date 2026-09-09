package database

import "testing"

// A module row whose ENABLED state follows a feature flag still owns its
// audience, and Settings -> Modules offers an All/Admin control for exactly
// that. seedSystemModules used to rewrite access_role on every boot, so the
// control worked, saved, and was put back the next time Core restarted - with
// nothing on screen saying so, and only on a deploy, which is the slowest
// possible way to find out.
//
// Position is checked alongside it because the same statements set it and the
// same screen advertises drag-to-reorder.
func TestOperatorModuleAudienceSurvivesTheNextBootIntegration(t *testing.T) {
	db := freshSchemaDB(t)

	// The two rows are set in OPPOSITE directions on purpose, because each boot
	// statement forced a different value: Tickets to 'all', Library to 'admin'.
	// A test that moved both the same way would agree with one of them by
	// accident and prove nothing about that row.
	//
	// Tickets stays admin-only, which is also where the seed leaves it - that is
	// the case the old code broke, by flipping it to 'all' on every start.
	// Library is opened to everyone, a real change away from its seed, which the
	// old code put back.
	if _, err := db.Exec(`UPDATE modules SET access_role = 'admin', position = 3 WHERE name = 'Tickets'`); err != nil {
		t.Fatalf("set tickets audience: %v", err)
	}
	if _, err := db.Exec(`UPDATE modules SET access_role = 'all' WHERE name = 'Library'`); err != nil {
		t.Fatalf("set library audience: %v", err)
	}

	// The next Core start.
	seedSystemModules(db)
	if err := applyDerivedModuleRows(db); err != nil {
		t.Fatalf("derive module rows: %v", err)
	}

	for _, tc := range []struct {
		module string
		role   string
	}{
		{"Tickets", "admin"},
		{"Library", "all"},
	} {
		var role string
		if err := db.QueryRow(`SELECT access_role FROM modules WHERE name = $1`, tc.module).Scan(&role); err != nil {
			t.Fatalf("read %s: %v", tc.module, err)
		}
		if role != tc.role {
			t.Errorf("%s audience is %q after a restart, want %q - the boot overwrote the operator's choice",
				tc.module, role, tc.role)
		}
	}

	var position int
	if err := db.QueryRow(`SELECT position FROM modules WHERE name = 'Tickets'`).Scan(&position); err != nil {
		t.Fatalf("read tickets position: %v", err)
	}
	if position != 3 {
		t.Errorf("Tickets sits at position %d after a restart, want 3 - the boot undid the reordering", position)
	}
}

// Tickets is a support surface, so it starts admin-only and the operator opens
// it up deliberately. The enabled state is not this test's business: that
// follows the ticket feature flag.
func TestFreshInstallSeedsTicketsAsAdminOnly(t *testing.T) {
	db := freshSchemaDB(t)

	var role string
	if err := db.QueryRow(`SELECT access_role FROM modules WHERE name = 'Tickets'`).Scan(&role); err != nil {
		t.Fatalf("read tickets: %v", err)
	}
	if role != "admin" {
		t.Errorf("a fresh install seeds Tickets as %q, want admin", role)
	}
}
