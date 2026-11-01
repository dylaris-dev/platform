package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// GetUserStoragePrefs returns the user's storage target per feature. A missing
// row reads as both nil.
func (s *PostgresStore) GetUserStoragePrefs(userID string) (backups, templates *int, err error) {
	var b, t sql.NullInt64
	err = s.db.QueryRow(`SELECT backups_storage_id, templates_storage_id FROM user_storage_prefs WHERE user_id = $1`, userID).Scan(&b, &t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("storage prefs: get %s: %w", userID, err)
	}
	return nullIntPtr(b), nullIntPtr(t), nil
}

// The setters below write ONE column each. Reading both and writing both back
// would let two saves (the Storage page and a storage marked default) undo each
// other's choice. None of them checks ownership: the caller does, and every
// reader re-checks it before using the storage.

// SetUserBackupsStoragePref chooses the backups target and moves the owner's
// own is_default flag with it, in one transaction. The flag has to follow: a
// NULL pref falls back to the owner's default storage in ResolveJobStorage, so
// "Dylaris storage" with a default still set would keep writing to the bucket.
func (s *PostgresStore) SetUserBackupsStoragePref(userID string, id *int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage prefs: backups %s: %w", userID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if _, err := tx.Exec(`INSERT INTO user_storage_prefs (user_id, backups_storage_id, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET backups_storage_id = EXCLUDED.backups_storage_id, updated_at = NOW()`,
		userID, id); err != nil {
		return fmt.Errorf("storage prefs: backups %s: %w", userID, err)
	}
	if _, err := tx.Exec(`UPDATE backup_storages SET is_default = COALESCE(id = $2, FALSE)
		WHERE owner_id = $1 AND is_default IS DISTINCT FROM COALESCE(id = $2, FALSE)`, userID, id); err != nil {
		return fmt.Errorf("storage prefs: backups default %s: %w", userID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage prefs: backups %s: %w", userID, err)
	}
	return nil
}

// SetUserTemplatesStoragePref chooses the templates target.
func (s *PostgresStore) SetUserTemplatesStoragePref(userID string, id *int) error {
	if _, err := s.db.Exec(`INSERT INTO user_storage_prefs (user_id, templates_storage_id, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET templates_storage_id = EXCLUDED.templates_storage_id, updated_at = NOW()`,
		userID, id); err != nil {
		return fmt.Errorf("storage prefs: templates %s: %w", userID, err)
	}
	return nil
}

// ClearUserBackupsStoragePrefIf returns backups to the chain only while the
// choice still names id, so a storage losing its default flag cannot undo a
// different choice made meanwhile.
func (s *PostgresStore) ClearUserBackupsStoragePrefIf(userID string, id int) error {
	if _, err := s.db.Exec(`UPDATE user_storage_prefs SET backups_storage_id = NULL, updated_at = NOW()
		WHERE user_id = $1 AND backups_storage_id = $2`, userID, id); err != nil {
		return fmt.Errorf("storage prefs: clear backups %s: %w", userID, err)
	}
	return nil
}
