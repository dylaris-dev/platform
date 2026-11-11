package database

import (
	"database/sql"
	"fmt"
)

// applyDataBudget adds the owner's self-set monthly data budget (Task 7 of the
// managed-gateway beam plan). A column on users rather than a settings row: it
// is one value per owner, read with nothing else, like the time preferences.
// TEXT because it holds the ParseLimitSetting sentinel ("" never set,
// "unlimited", or GB). Additive with a default, so an older Core replica keeps
// working and simply never gates.
func applyDataBudget(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS data_budget_gb TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("users data_budget_gb: %w", err)
	}
	return nil
}
