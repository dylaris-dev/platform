package database

import (
	"database/sql"
	"fmt"
)

// applyPhase15Schema sets up the User UUID migration + username
// history tables. The users.id UUID + drop of public_id happen in
// createUsersTable; this function only adds the new history table +
// settings rows. Idempotent.
func applyPhase15Schema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS user_username_history (
		id           SERIAL PRIMARY KEY,
		user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		old_username VARCHAR(64) NOT NULL,
		new_username VARCHAR(64) NOT NULL,
		changed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		changed_by   UUID REFERENCES users(id) ON DELETE SET NULL
	)`); err != nil {
		return fmt.Errorf("phase 15: create user_username_history: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_username_history_user
		ON user_username_history(user_id, changed_at DESC)`); err != nil {
		return fmt.Errorf("phase 15: create username_history index: %w", err)
	}
	for _, q := range []string{
		`INSERT INTO settings (key, value) VALUES ('users.allow_name_change', 'true') ON CONFLICT (key) DO NOTHING`,
		`INSERT INTO settings (key, value) VALUES ('users.name_change_cooldown_days', '30') ON CONFLICT (key) DO NOTHING`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("phase 15: seed settings: %w", err)
		}
	}
	return nil
}

// applyPhase16Schema sets up the Modpack Storage + Feature Toggle schema:
//   - users.can_create_modpacks per-user gate (default true)
//   - Default settings rows for feature toggle + storage config
//
// Idempotent. The old modpack_versions column ALTERs were dropped with the
// unified pack migration — those tables no longer exist after boot, so the
// per-user gate + settings seeding are all that remains here. The unified pack
// builder reuses both.
func applyPhase16Schema(db *sql.DB) error {
	// users.can_create_modpacks
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS can_create_modpacks BOOLEAN NOT NULL DEFAULT TRUE`); err != nil {
		return fmt.Errorf("phase 16: add can_create_modpacks: %w", err)
	}
	// Seed default settings rows. ON CONFLICT keeps existing values on re-boot,
	// so this only sets the default for a brand-new/unset install; existing
	// instances keep whatever they already stored.
	for _, kv := range []struct{ k, v string }{
		{"feature_modpacks_enabled", "false"},
		{"modpack_storage_provider", "local"},
		{"modpack_storage_paths", "[]"},
		{"modpack_storage_s3_endpoint", ""},
		{"modpack_storage_s3_bucket", ""},
		{"modpack_storage_s3_region", ""},
		{"modpack_storage_s3_access_key", ""},
		{"modpack_storage_s3_secret_key", ""},
	} {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO NOTHING`, kv.k, kv.v); err != nil {
			return fmt.Errorf("phase 16: seed settings: %w", err)
		}
	}
	return nil
}

// applyPhase14Schema now only sets up the modrinth_pats table (encrypted
// per-user Modrinth PAT). The old Phase 14 modpacks/modpack_versions/
// modpack_mods tables were retired with the unified pack model — they are
// dropped once in applyUnifiedModpackSchema, so recreating them here would
// only churn (create then drop) on every boot. modrinth_pats survives because
// Modrinth publishing (a later phase) reuses it.
func applyPhase14Schema(db *sql.DB) error {
	for _, q := range []string{
		// PAT storage. ciphertext is hex(aes-gcm(plaintext)). nonce kept
		// inline (12 bytes prefix). One row per user — replacing the PAT
		// overwrites. Username + last_validated_at let the UI show
		// "Connected as <username> (valid as of ...)".
		`CREATE TABLE IF NOT EXISTS modrinth_pats (
			user_id           UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			ciphertext        TEXT        NOT NULL,
			modrinth_username VARCHAR(128) NOT NULL DEFAULT '',
			last_validated_at TIMESTAMPTZ,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("phase 14: %w", err)
		}
	}
	return nil
}
