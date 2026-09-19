package database

import (
	"database/sql"
	"fmt"
)

// applyPlansSchema creates the BYON plan tiers + links users to a plan + adds the
// per-user limit override columns to user_billing. Idempotent.
//
// Limits that matter for BYON (the tenant brings their own node hardware, so no
// server-count/RAM caps): max_nodes (hard), r2_quota_gb (hard, reuses the existing
// backup gate), and monthly traffic (edge / relay / combined, WARN-only). 0 means
// unlimited everywhere. A plan's value is the baseline; a per-user override in
// user_billing wins. Everything here only takes effect when feature_byon_enabled.
func applyPlansSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS plans (
		id                  SERIAL PRIMARY KEY,
		name                TEXT NOT NULL,
		price_label         TEXT NOT NULL DEFAULT '',
		max_nodes           INTEGER NOT NULL DEFAULT 0,
		r2_quota_gb         BIGINT  NOT NULL DEFAULT 0,
		traffic_edge_gb     BIGINT  NOT NULL DEFAULT 0,
		traffic_relay_gb    BIGINT  NOT NULL DEFAULT 0,
		traffic_combined_gb BIGINT  NOT NULL DEFAULT 0,
		is_default          BOOLEAN NOT NULL DEFAULT FALSE,
		created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("plans: create plans: %w", err)
	}
	// At most one default plan.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_plans_one_default
		ON plans(is_default) WHERE is_default`); err != nil {
		return fmt.Errorf("plans: default index: %w", err)
	}

	// Link users to a plan (NULL -> the default plan at resolve time).
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_id INTEGER REFERENCES plans(id)`); err != nil {
		return fmt.Errorf("plans: add users.plan_id: %w", err)
	}

	// max_links is stored with a plan but caps nothing: plans no longer take part
	// in limits, see services.EffectiveLimits.
	if _, err := db.Exec(`ALTER TABLE plans ADD COLUMN IF NOT EXISTS max_links INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("plans: add plans.max_links: %w", err)
	}

	// Per-user limit overrides live in user_billing alongside the existing
	// r2_quota_gb override (NULL = use the plan's value).
	for _, col := range []string{
		`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS max_nodes INTEGER`,
		`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS max_links INTEGER`,
		// Separate consent from the traffic one on purpose: agreeing to pay for
		// player traffic is not agreeing to pay for stored backups, and one
		// switch for both would enrol somebody in a charge they never saw.
		`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS backup_billing_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS traffic_edge_gb BIGINT`,
		`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS traffic_relay_gb BIGINT`,
		`ALTER TABLE user_billing ADD COLUMN IF NOT EXISTS traffic_combined_gb BIGINT`,
	} {
		if _, err := db.Exec(col); err != nil {
			return fmt.Errorf("plans: user_billing override col: %w", err)
		}
	}
	return nil
}
