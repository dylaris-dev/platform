package database

import (
	"database/sql"
	"fmt"
)

// applyAPIKeyAudit lets an audit row name the API key that acted. The owner
// stays in actor_user_id (the accountability chain); the key id and a snapshot
// of its name sit beside it, so a later rename or revocation never rewrites
// history. No foreign key: a key row is never deleted, and the audit row must
// outlive a hard-deleted owner whose keys cascade away.
func applyAPIKeyAudit(db *sql.DB) error {
	for _, table := range []string{"server_audit_events", "audit_events_identity"} {
		if _, err := db.Exec(`ALTER TABLE ` + table + `
			ADD COLUMN IF NOT EXISTS actor_api_key_id INTEGER,
			ADD COLUMN IF NOT EXISTS actor_api_key_name VARCHAR(64)`); err != nil {
			return fmt.Errorf("%s api key columns: %w", table, err)
		}
	}
	return nil
}
