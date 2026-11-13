package database

import (
	"database/sql"
	"fmt"
)

// applyRemoteKitRetire records, on a customer Core, when the provider revokes a
// node's link kit that the operator retired (managed gateway stage 7c). NULL is
// a kit in normal use. ADD COLUMN without a default is catalog-only; the table
// holds one row per node.
func applyRemoteKitRetire(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE remote_link_kits ADD COLUMN IF NOT EXISTS retire_at TIMESTAMPTZ NULL`); err != nil {
		return fmt.Errorf("remote kit retirement column: %w", err)
	}
	return nil
}
