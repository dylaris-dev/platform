package database

import (
	"database/sql"
	"fmt"
)

// applyTicketCategorySnapshot lets a ticket outlive the category it was filed
// under.
//
// tickets.category_id was NOT NULL with ON DELETE RESTRICT, so a category could
// never be deleted once used - DeleteCategory answered 409 "disable it instead".
// That protected the tickets, but it made the first five minutes of an install
// permanent: a self-hoster who mistypes a category name while setting up is
// stuck with it forever, in a list every customer reads, with no way out short
// of editing the database by hand.
//
// The category NAME is now copied onto the ticket when it is filed. A ticket
// therefore carries what it was filed under as TEXT, and stops caring whether
// that row still exists. The id stays for filtering and for the colour, and
// goes NULL when the category is deleted.
//
// SET NULL rather than CASCADE, for the reason applyInviteAttributionNullable
// gives: CASCADE here would delete the customer's ticket history because an
// operator tidied a category, which is data loss dressed as cleanup.
//
// The reader was an INNER JOIN on the category (ticketBaseFrom), so a NULL id
// would have made the ticket VANISH from every list while the row still
// existed - worse than the restriction it replaces. It is a LEFT JOIN now, and
// the name is read from the snapshot rather than the join.
func applyTicketCategorySnapshot(db *sql.DB) error {
	if _, err := db.Exec(
		`ALTER TABLE tickets ADD COLUMN IF NOT EXISTS category_name TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("ticket category snapshot: add tickets.category_name: %w", err)
	}
	// Backfill BEFORE the constraint changes, while every ticket still has a
	// category to copy from. Doing it afterwards would be a race with the first
	// operator who deletes one.
	if _, err := db.Exec(`
		UPDATE tickets t SET category_name = c.name
		  FROM ticket_categories c
		 WHERE t.category_id = c.id AND t.category_name = ''`); err != nil {
		return fmt.Errorf("ticket category snapshot: backfill: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE tickets ALTER COLUMN category_id DROP NOT NULL`); err != nil {
		return fmt.Errorf("ticket category snapshot: drop NOT NULL on tickets.category_id: %w", err)
	}
	// Drop-then-add for idempotency: ADD CONSTRAINT has no IF NOT EXISTS, and
	// this is Postgres's own default name for the column, so it matches a fresh
	// install and every existing one.
	if _, err := db.Exec(`ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_category_id_fkey`); err != nil {
		return fmt.Errorf("ticket category snapshot: drop the old tickets.category_id constraint: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE tickets
		ADD CONSTRAINT tickets_category_id_fkey
		FOREIGN KEY (category_id) REFERENCES ticket_categories(id) ON DELETE SET NULL`); err != nil {
		return fmt.Errorf("ticket category snapshot: re-add tickets.category_id as ON DELETE SET NULL: %w", err)
	}
	return nil
}
