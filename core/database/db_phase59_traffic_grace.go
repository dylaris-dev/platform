package database

import (
	"database/sql"
	"fmt"
)

// applyTrafficGrace adds the data grace bookkeeping to traffic_usage: per owner
// per month, the relay bytes kept out of a managed pool (owner decision
// 2026-11-11). A column pair on the existing row rather than a table: the key
// is the same (user_id, period), the month rollover is the same, and the
// grace state is read with the usage it is subtracted from. Additive with
// defaults, so an older Core replica during a rollout keeps working and simply
// never excludes anything.
func applyTrafficGrace(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE traffic_usage
		ADD COLUMN IF NOT EXISTS grace_excluded_bytes BIGINT NOT NULL DEFAULT 0,
		ADD COLUMN IF NOT EXISTS grace_since_relay BIGINT`); err != nil {
		return fmt.Errorf("traffic_usage grace columns: %w", err)
	}
	return nil
}
