package database

import (
	"database/sql"
	"fmt"
)

// applyBackupJobTimezone adds the IANA zone a backup schedule is read in
// ("daily 03:00" means 03:00 somewhere). Existing rows get UTC, which changes
// nothing for them: every schedule saved before this is relative ("every 6h")
// and never reads a zone.
func applyBackupJobTimezone(db *sql.DB) error {
	for _, table := range []string{"backup_jobs", "platform_backup_jobs"} {
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC'`); err != nil {
			return fmt.Errorf("%s timezone schema: %w", table, err)
		}
	}
	return nil
}
