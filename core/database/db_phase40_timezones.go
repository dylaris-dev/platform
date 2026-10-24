package database

import (
	"database/sql"
	"fmt"
)

// applyTimezoneSchema adds the time zone a person reads times in and the one a
// server runs in.
//
// users.timezone and servers.timezone are empty for "not chosen": a server
// without one inherits its owner's, and an owner without one means UTC, which
// is what everything ran in before. scheduled_tasks.timezone is the zone the
// task's cron is read in, stored per task so a later change of the account or
// server zone does not silently move a schedule someone already wrote. Rows
// that existed before this column were written against UTC, hence the default.
func applyTimezoneSchema(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS time_format TEXT NOT NULL DEFAULT 'auto'`,
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE scheduled_tasks ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC'`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("timezone schema: %w", err)
		}
	}
	return nil
}
