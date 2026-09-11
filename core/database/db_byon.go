package database

import (
	"database/sql"
	"fmt"
)

// applyBYONSchema creates the BYON (bring-your-own-node) tables. Idempotent.
// Only used when feature_byon_enabled; the tables are harmless when the feature
// is off (no rows, never read).
func applyBYONSchema(db *sql.DB) error {
	// node_enroll_tokens: per-user tokens a tenant uses to enroll their own node.
	// The plaintext token is only ever returned once at mint time; we store a
	// hash. Single-use (consumed on enroll, see ConsumeNodeEnrollToken) until
	// expires_at (NULL = no expiry); revoked by deleting the row. The plan's
	// max_nodes is enforced separately at enroll time.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS node_enroll_tokens (
		id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash  TEXT NOT NULL UNIQUE,
		label       TEXT NOT NULL DEFAULT '',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at  TIMESTAMPTZ,
		consumed_at TIMESTAMPTZ
	)`); err != nil {
		return fmt.Errorf("byon: create node_enroll_tokens: %w", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_node_enroll_tokens_user ON node_enroll_tokens(user_id)`)
	db.Exec(`ALTER TABLE node_enroll_tokens ADD COLUMN IF NOT EXISTS consumed_at TIMESTAMPTZ`)
	// P0b-5 recovery: a recovery token re-pairs an EXISTING node identity (nodes.token).
	// Plain column, no FK (nodes.token is unique but not a foreign key); validated live at consume.
	db.Exec(`ALTER TABLE node_enroll_tokens ADD COLUMN IF NOT EXISTS recovers_node_token TEXT`)
	// The BYON node key (warp_api_keys.node_id) minted for the same machine as
	// this token. When the token is redeemed, that key is bound to the node it
	// created (store.BindWarpKeyFromEnrollToken). Plain column like the one above:
	// the key row is re-checked at bind time, so a key revoked or bound in the
	// meantime is simply not bound. Checked, unlike its neighbours: without it
	// every mint's INSERT fails.
	if _, err := db.Exec(`ALTER TABLE node_enroll_tokens ADD COLUMN IF NOT EXISTS warp_key_node_id TEXT`); err != nil {
		return fmt.Errorf("byon: add node_enroll_tokens.warp_key_node_id: %w", err)
	}

	// P0b-5 admission: global-scope IP allowlist for NEW node registrations.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS node_admission_cidrs (
		id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		cidr       TEXT NOT NULL,
		label      TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("byon: create node_admission_cidrs: %w", err)
	}
	return nil
}
