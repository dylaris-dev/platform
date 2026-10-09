package database

import (
	"database/sql"
	"fmt"
)

// applyPendingEmailSchema holds an address a user asked for and has not yet
// confirmed. A profile change used to replace the address and drop the
// verified mark at once, so a typo locked the account out at its next sign-in
// with the confirmation on its way to a mailbox nobody reads. The old address
// now stays in force until the new one answers.
func applyPendingEmailSchema(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS pending_email TEXT`); err != nil {
		return fmt.Errorf("users: add pending_email: %w", err)
	}
	return nil
}
