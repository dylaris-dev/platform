package database

import (
	"database/sql"
	"fmt"
)

// applyAuthzFoundationSchema scaffolds the permission-system redesign (phase 1).
// It is ADDITIVE ONLY: it creates the new panel_roles / server_roles tables,
// adds nullable authz columns to users + server_invites, and seeds the
// permissions_mode setting. It does NOT drop is_admin / role / can_* or the old
// server_invites.permissions column - those stay until the phase-4 migration of
// old invite blobs. Every statement is idempotent.
func applyAuthzFoundationSchema(db *sql.DB) error {
	// panel_roles: level-1 staff role = bundle of PANEL caps.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS panel_roles (
		id            SERIAL PRIMARY KEY,
		name          VARCHAR(64)  NOT NULL UNIQUE,
		capabilities  JSONB        NOT NULL DEFAULT '[]'::jsonb,
		is_system     BOOLEAN      NOT NULL DEFAULT FALSE,
		created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
		created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("authz foundation: create panel_roles: %w", err)
	}

	// server_roles: owner-scoped, reusable bundle of SERVER + OWNER caps.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_roles (
		id            SERIAL PRIMARY KEY,
		owner_user_id UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name          VARCHAR(64)  NOT NULL,
		capabilities  JSONB        NOT NULL DEFAULT '[]'::jsonb,
		created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
		UNIQUE(owner_user_id, name)
	)`); err != nil {
		return fmt.Errorf("authz foundation: create server_roles: %w", err)
	}

	// users: nullable panel role FK + per-user override set.
	for _, q := range []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS panel_role_id INTEGER REFERENCES panel_roles(id) ON DELETE SET NULL`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS panel_cap_overrides JSONB NOT NULL DEFAULT '{}'::jsonb`,
		// Bumped to end every session of the account at once ("sign out
		// everywhere", 2FA on/off, an admin's 2FA reset). Folded into the
		// session fingerprint, so 0 keeps every existing session valid.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS session_epoch INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("authz foundation: alter users: %w", err)
		}
	}

	// server_invites rework (ADDITIVE): server_id becomes nullable (account-wide
	// grant), plus the new owner_user_id / server_role_id / cap_overrides /
	// inherit columns. The legacy permissions JSONB column is LEFT IN PLACE for
	// the phase-4 migration. DROP NOT NULL is a no-op if already nullable.
	for _, q := range []string{
		`ALTER TABLE server_invites ALTER COLUMN server_id DROP NOT NULL`,
		`ALTER TABLE server_invites ADD COLUMN IF NOT EXISTS owner_user_id  UUID REFERENCES users(id) ON DELETE CASCADE`,
		`ALTER TABLE server_invites ADD COLUMN IF NOT EXISTS server_role_id INTEGER REFERENCES server_roles(id) ON DELETE SET NULL`,
		`ALTER TABLE server_invites ADD COLUMN IF NOT EXISTS cap_overrides  JSONB NOT NULL DEFAULT '{}'::jsonb`,
		`ALTER TABLE server_invites ADD COLUMN IF NOT EXISTS inherit        BOOLEAN NOT NULL DEFAULT FALSE`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("authz foundation: alter server_invites: %w", err)
		}
	}

	// permissions_mode setting: off|simple|advanced. Fresh installs default to
	// 'simple' (spec). ON CONFLICT DO NOTHING keeps an operator's chosen value.
	if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('permissions_mode', 'simple')
		ON CONFLICT (key) DO NOTHING`); err != nil {
		return fmt.Errorf("authz foundation: seed permissions_mode: %w", err)
	}
	return nil
}
