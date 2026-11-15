package database

import (
	"database/sql"
	"fmt"
)

// applyAddressDNSChecks creates the last DNS pointing check of an address on a
// customer's own domain: one row per name, shared by every use that judges
// CNAME/A/AAAA the same (Bedrock, the white-label voice name). Only a verdict
// is ever stored, never "could not check" (store.RecordAddressDNSCheck refuses
// it). failing_since and misses describe the current run of definitive misses
// and are reset by the next ok.
//
// Locks: a new table and an index on it, nothing existing is touched.
func applyAddressDNSChecks(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS address_dns_checks (
			name          TEXT        PRIMARY KEY,
			status        TEXT        NOT NULL,
			region        TEXT        NOT NULL DEFAULT '',
			checked_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			failing_since TIMESTAMPTZ,
			misses        INTEGER     NOT NULL DEFAULT 0
		)`,
		// The re-check asks for the oldest rows first.
		`CREATE INDEX IF NOT EXISTS idx_address_dns_checks_checked_at ON address_dns_checks (checked_at)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("address dns checks: %w", err)
		}
	}
	return nil
}
