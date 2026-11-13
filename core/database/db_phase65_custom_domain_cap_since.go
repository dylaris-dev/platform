package database

import (
	"database/sql"
	"fmt"
)

// applyCustomDomainCapSince records when a store activate removed a tenant's
// custom-domain row, i.e. when their managed plan ended. Only those tenants
// have their own-domain routes counted by the over-limit sweep. NULL for
// everyone else. ADD COLUMN without a default is catalog-only: a brief ACCESS
// EXCLUSIVE lock on user_billing, no rewrite.
func applyCustomDomainCapSince(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS custom_domain_cap_since TIMESTAMPTZ NULL`); err != nil {
		return fmt.Errorf("custom-domain cap marker column: %w", err)
	}
	return nil
}
