package database

import (
	"testing"
)

// A fresh install must be able to receive a support ticket.
//
// With the table empty, CreateTicket looks the category up and answers "Unknown
// or disabled category" to EVERY attempt - so an empty table is not an empty
// list, it is a closed inbox with nothing anywhere saying so. Production ran in
// exactly that state with registration open.
func TestFreshInstallHasTicketCategories(t *testing.T) {
	db := freshSchemaDB(t)

	rows, err := db.Query(`SELECT name, enabled, default_priority FROM ticket_categories ORDER BY position`)
	if err != nil {
		t.Fatalf("read seeded categories: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name, priority string
		var enabled bool
		if err := rows.Scan(&name, &enabled, &priority); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !enabled {
			t.Errorf("category %q is seeded disabled, so it cannot be chosen", name)
		}
		if !validSeedPriority(priority) {
			t.Errorf("category %q has priority %q, which CreateCategory would refuse", name, priority)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("a fresh install has no ticket categories, so no user can open a ticket")
	}
	if len(names) != len(defaultTicketCategorySeeds()) {
		t.Errorf("seeded %d categories %v, want %d", len(names), names, len(defaultTicketCategorySeeds()))
	}
}

// Re-running the seed must not duplicate rows, and must not resurrect what an
// operator deleted. Both matter on a restart, which is the only time this runs.
func TestFreshInstallTicketCategorySeedRespectsTheOperator(t *testing.T) {
	db := freshSchemaDB(t)

	// A second boot changes nothing.
	if err := seedDefaultTicketCategories(db); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ticket_categories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if want := len(defaultTicketCategorySeeds()); n != want {
		t.Fatalf("after a second boot there are %d categories, want %d", n, want)
	}

	// An operator prunes ours down to one of their own and restarts. Seeding by
	// NAME would put all five back here, and they would have no way to make the
	// deletion stick.
	if _, err := db.Exec(`DELETE FROM ticket_categories`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO ticket_categories (name, default_priority, enabled, position)
		 VALUES ('Operator only', 'normal', TRUE, 1)`); err != nil {
		t.Fatalf("insert the operator's own: %v", err)
	}
	if err := seedDefaultTicketCategories(db); err != nil {
		t.Fatalf("seed after the operator pruned: %v", err)
	}
	var after int
	if err := db.QueryRow(`SELECT count(*) FROM ticket_categories`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 1 {
		t.Errorf("the operator kept 1 category and a restart left %d: deleted defaults came back", after)
	}

	// But a table emptied completely is a closed inbox, and that IS worth
	// fixing on the next boot.
	if _, err := db.Exec(`DELETE FROM ticket_categories`); err != nil {
		t.Fatalf("clear again: %v", err)
	}
	if err := seedDefaultTicketCategories(db); err != nil {
		t.Fatalf("seed into an empty table: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM ticket_categories`).Scan(&after); err != nil {
		t.Fatalf("count after refill: %v", err)
	}
	if after != len(defaultTicketCategorySeeds()) {
		t.Errorf("an empty table was left empty (%d rows): the support inbox stays closed", after)
	}
}

func validSeedPriority(p string) bool {
	return p == "low" || p == "normal" || p == "high" || p == "urgent"
}
