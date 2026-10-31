package database

import (
	"database/sql"
	"fmt"
)

// applyMinecraftEulaSchema records when a user accepted the Minecraft EULA.
// The node writes eula=true into every server it installs, which is a
// declaration the person running the server has to make; this is the record
// that they made it. NULL = not yet, so setup asks before the first install.
func applyMinecraftEulaSchema(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS minecraft_eula_accepted_at TIMESTAMPTZ`); err != nil {
		return fmt.Errorf("minecraft eula schema: %w", err)
	}
	return nil
}
