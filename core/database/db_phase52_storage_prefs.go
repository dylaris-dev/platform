package database

import (
	"database/sql"
	"fmt"
)

// applyStoragePrefs adds the per-user storage choice (backups, templates; NULL
// = Dylaris storage) and records on each template which tenant storage holds
// it (NULL = ours).
//
// The storage references use the default NO ACTION: a storage something still
// names is not deleted. Unlike RESTRICT, NO ACTION is checked at the end of
// the statement, so account deletion works whatever order the cascades fire
// in: the user's prefs and templates go in the same statement as their
// storages (pinned by TestIntegrationUserStoragePrefs).
//
// The backfill copies a tenant's current own default into backups_storage_id,
// so the job resolution keeps choosing what it chose before. Only for users
// without a row: a row is the user's own later choice.
func applyStoragePrefs(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS user_storage_prefs (
			user_id              UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			backups_storage_id   INTEGER REFERENCES backup_storages(id),
			templates_storage_id INTEGER REFERENCES backup_storages(id),
			updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE templates ADD COLUMN IF NOT EXISTS backup_storage_id INTEGER REFERENCES backup_storages(id)`,
		`CREATE INDEX IF NOT EXISTS idx_templates_backup_storage ON templates(backup_storage_id) WHERE backup_storage_id IS NOT NULL`,
		`INSERT INTO user_storage_prefs (user_id, backups_storage_id)
			SELECT owner_id, id FROM backup_storages
			WHERE owner_id IS NOT NULL AND is_default
			ON CONFLICT (user_id) DO NOTHING`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("storage prefs schema: %w", err)
		}
	}
	return nil
}
