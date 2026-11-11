package database

import (
	"database/sql"
	"fmt"
)

// applyBeamIssuerKeys creates the registry of managed-gateway customers' beam
// ticket keys. A revoked row stays (revoked_at set) so the publisher keeps
// deleting its Redis record; an account deletion cascades the rows away and the
// reconcile loop sweeps the records nothing owns any more.
func applyBeamIssuerKeys(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS beam_issuer_keys (
		kid         TEXT        PRIMARY KEY,
		owner_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		public_key  BYTEA       NOT NULL CHECK (octet_length(public_key) = 32),
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		revoked_at  TIMESTAMPTZ
	)`); err != nil {
		return fmt.Errorf("beam_issuer_keys: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS beam_issuer_keys_owner_idx
		ON beam_issuer_keys (owner_id)`); err != nil {
		return fmt.Errorf("beam_issuer_keys owner index: %w", err)
	}
	return nil
}
