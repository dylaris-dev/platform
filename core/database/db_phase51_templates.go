package database

import (
	"database/sql"
	"fmt"
)

// applyTemplatesSchema adds the Templates tables: user-owned starting points
// for servers and proxies, per-template shares, and owner-wide shares that
// cover every template an owner has (now and later). All of it dies with the
// owning user.
func applyTemplatesSchema(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS templates (
			id                    SERIAL PRIMARY KEY,
			owner_id              UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name                  TEXT NOT NULL,
			kind                  TEXT NOT NULL CHECK (kind IN ('server', 'proxy')),
			visibility            TEXT NOT NULL DEFAULT 'personal' CHECK (visibility IN ('personal', 'admin', 'platform')),
			software              TEXT NOT NULL DEFAULT '',
			mc_version            TEXT NOT NULL DEFAULT '',
			software_version      TEXT NOT NULL DEFAULT '',
			storage_connection_id INTEGER NOT NULL DEFAULT 0,
			size_bytes            BIGINT NOT NULL DEFAULT 0,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_templates_owner ON templates(owner_id)`,
		`CREATE TABLE IF NOT EXISTS template_shares (
			template_id INTEGER NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
			user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			level       TEXT NOT NULL CHECK (level IN ('use', 'edit')),
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (template_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS template_owner_shares (
			owner_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			level      TEXT NOT NULL CHECK (level IN ('use', 'edit')),
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (owner_id, user_id)
		)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("templates schema: %w", err)
		}
	}
	return nil
}
