package database

import (
	"database/sql"
	"fmt"
)

// applyMemoryGuardSchema adds what the node's memory guard reports into:
// memory_guard_action is what Core does when a server holds near its container
// limit ('off' by default: warn only; 'stop' or 'restart'), and last_crash_* records the
// last OOM kill so the panel can say why a server went down.
func applyMemoryGuardSchema(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS memory_guard_action TEXT NOT NULL DEFAULT 'off'`,
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS last_crash_reason TEXT`,
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS last_crash_at TIMESTAMPTZ`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("servers: memory guard columns: %w", err)
		}
	}
	return nil
}
