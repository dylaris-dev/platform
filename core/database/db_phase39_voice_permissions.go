package database

import (
	"database/sql"
	"fmt"
)

// applyVoicePermissionsSchema adds the scoped answer to "may this server turn
// voice chat on". scope is server:<id>, user:<id> or role:<panel role id>; the
// most specific row wins and a missing row defers to the next scope down. The
// broadest scope is not a row: it is the setting gateway.voice_chat_enabled.
func applyVoicePermissionsSchema(db *sql.DB) error {
	q := `CREATE TABLE IF NOT EXISTS voice_permissions (
		scope      TEXT PRIMARY KEY,
		allowed    BOOLEAN NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("voice permissions schema: %w", err)
	}
	return nil
}
