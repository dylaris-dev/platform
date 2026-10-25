package database

import (
	"database/sql"
	"fmt"
)

// applyRevokedSessionsSchema adds the durable list of signed-out sessions.
// Redis held it alone, with no persistence, so a Redis restart brought every
// individually signed-out session back to life for the rest of its lifetime.
// Redis stays the fast path; this table is what survives it. A row is useless
// after expires_at (no copy of the session can be alive), and the next
// sign-out deletes those.
func applyRevokedSessionsSchema(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS revoked_sessions (
			id         TEXT PRIMARY KEY,
			expires_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS revoked_sessions_expires ON revoked_sessions (expires_at)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("revoked sessions schema: %w", err)
		}
	}
	return nil
}
