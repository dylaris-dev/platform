package database

import (
	"database/sql"
	"fmt"
)

// applyDataBillingSchema splits the traffic consent: traffic_billing_enabled
// is now PLAYER traffic, data_billing_enabled DATA traffic (beam + custom
// tabs). NULL follows the old single consent, so a store that does not send
// the split yet keeps meaning what it meant: one consent for everything.
func applyDataBillingSchema(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS data_billing_enabled BOOLEAN`); err != nil {
		return fmt.Errorf("data billing schema: %w", err)
	}
	return nil
}
