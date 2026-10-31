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

	// Both directions on purpose: the old boot statements forced one value per
	// row, so a test that set only one would agree with that statement by
	// accident and prove nothing. 'admin' is also where the seed leaves Tickets
	// - the case the old code broke, by flipping it to 'all' on every start.
	for _, role := range []string{"all", "admin"} {
		if _, err := db.Exec(`UPDATE modules SET access_role = $1, position = 3 WHERE name = 'Tickets'`, role); err != nil {
			t.Fatalf("set tickets audience: %v", err)
		}

		// The next Core start.
		seedSystemModules(db)
		if err := applyDerivedModuleRows(db); err != nil {
			t.Fatalf("derive module rows: %v", err)
		}

		var got string
		var position int
		if err := db.QueryRow(`SELECT access_role, position FROM modules WHERE name = 'Tickets'`).Scan(&got, &position); err != nil {
			t.Fatalf("read tickets: %v", err)
		}
		if got != role {
			t.Errorf("Tickets audience is %q after a restart, want %q - the boot overwrote the operator's choice", got, role)
		}
		if position != 3 {
			t.Errorf("Tickets sits at position %d after a restart, want 3 - the boot undid the reordering", position)
		}
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
