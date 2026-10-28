package database

import (
	"database/sql"
	"fmt"
)

// applyModrinthImportSchema adds what importing existing Modrinth modpacks
// needs: a mark on builds that came from Modrinth rather than from this panel,
// and the per-user list of Modrinth projects hidden from the import list.
func applyModrinthImportSchema(db *sql.DB) error {
	for _, statement := range []string{
		`ALTER TABLE pack_builds ADD COLUMN IF NOT EXISTS imported_from VARCHAR(16) NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS modrinth_hidden_projects (
		 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		 project_id VARCHAR(64) NOT NULL,
		 PRIMARY KEY(user_id, project_id))`,
		// Access to a Modrinth-bound pack comes from the Modrinth team; this is
		// the last answer Modrinth gave per user and project (state member,
		// none or token_invalid), so a page load does not ask Modrinth again.
		`CREATE TABLE IF NOT EXISTS modrinth_memberships (
		 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		 project_id VARCHAR(64) NOT NULL,
		 permissions BIGINT NOT NULL DEFAULT 0,
		 state VARCHAR(16) NOT NULL,
		 checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		 PRIMARY KEY(user_id, project_id))`,
		`ALTER TABLE modrinth_memberships ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ`,
		// One Modrinth project is one pack: team members share it.
		`CREATE UNIQUE INDEX IF NOT EXISTS packs_modrinth_project_unique ON packs(modrinth_project_id) WHERE modrinth_project_id <> ''`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("modrinth import schema: %w", err)
		}
	}
	return nil
}
