package database

import (
	"database/sql"
	"fmt"
)

// applyKitRetire adds link kit retirement (managed gateway stage 7c): a kit its
// owner retires stops counting toward the link cap at once and is revoked at
// warp_api_keys.retire_at.
//
//   - retire_at: NULL for every existing row. ADD COLUMN without a default is
//     a catalog-only change, so the hot table is not rewritten; it still takes
//     an ACCESS EXCLUSIVE lock for that instant.
//   - idx_warp_api_keys_retire_at: partial, so it holds only the retiring rows
//     the minute tick asks for. Built non-concurrently (migrations run as plain
//     statements at boot); that takes a SHARE lock, blocking writes to
//     warp_api_keys for one scan of a table of kits and node keys.
//   - warp_kit_retirements: one row per retirement, kept after the kit is
//     revoked, because the monthly retire budget counts them. Purged after 90
//     days by the same tick.
func applyKitRetire(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE warp_api_keys ADD COLUMN IF NOT EXISTS retire_at TIMESTAMPTZ`,
		`CREATE INDEX IF NOT EXISTS idx_warp_api_keys_retire_at ON warp_api_keys (retire_at) WHERE retire_at IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS warp_kit_retirements (
			owner_id   TEXT NOT NULL,
			link_id    TEXT NOT NULL,
			retired_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (link_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_warp_kit_retirements_owner ON warp_kit_retirements (owner_id, retired_at)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("kit retirement schema: %w", err)
		}
	}
	return nil
}
