package database

import (
	"database/sql"
	"fmt"
)

// applyLibraryRemoval drops what the removed Library feature left in the
// database: its per-path denylist, its navbar row and its feature flag. Every
// statement is a no-op once it has run, so it stays safe on every boot.
//
// The files themselves live in Core storage, not here; AppState.PurgeLibraryStorage
// deletes those once the storage config is known.
func applyLibraryRemoval(db *sql.DB) error {
	for _, q := range []string{
		`DROP TABLE IF EXISTS library_disabled`,
		`DELETE FROM modules WHERE name = 'Library' AND url = '/library'`,
		`DELETE FROM settings WHERE key = 'feature_library_enabled'`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("library removal: %w", err)
		}
	}
	return nil
}
