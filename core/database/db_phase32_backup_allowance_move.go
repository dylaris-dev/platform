package database

import (
	"database/sql"
	"fmt"
	"log"
)

// applyBackupAllowanceSettingMove moves the platform backup allowance out of the
// Billing screen and into Settings, Backups.
//
// billing.r2_quota_gb answered "how much backup storage does an owner with no
// entitlement get". The Billing tab is hidden without a hosted store, so on a
// self-hosted install that control sat on a page the operator could not open,
// while the guard that reads it ran on every backup. It is now
// backup.default_user_quota_gb, next to the other backup policy.
//
// A stored "0" is deliberately NOT carried across. Under the limit convention 0
// is a real cap of NONE, and the Billing GET used to answer "0" for an unset
// quota while its own PUT wrote whatever the panel sent back - so opening that
// screen once and pressing Save stored a platform-wide refusal. MEASURED in this
// deployment: billing.r2_quota_gb held "0", written in one batch with its
// sibling keys, and every server whose owner held no entitlement was refused
// with "backup quota reached (0 / 0 GB used)". Treating that 0 as a deliberate
// answer would carry a bug forward as policy. An operator who really wants a cap
// of none types it again on the new screen, where the field says what 0 means.
//
// Idempotent, and it never overwrites: a value already under the new key has
// been decided since the move and wins over anything the old one still says.
func applyBackupAllowanceSettingMove(db *sql.DB) error {
	const (
		oldKey = "billing.r2_quota_gb"
		newKey = "backup.default_user_quota_gb"
	)

	var moved string
	err := db.QueryRow(`
		INSERT INTO settings (key, value, updated_at)
		SELECT $1, s.value, NOW() FROM settings s
		 WHERE s.key = $2 AND s.value <> '' AND s.value <> '0'
		ON CONFLICT (key) DO NOTHING
		RETURNING value`, newKey, oldKey).Scan(&moved)
	switch {
	case err == sql.ErrNoRows:
		// Nothing to carry, or the new key already holds an answer. Both fine.
	case err != nil:
		return fmt.Errorf("backup allowance move: insert: %w", err)
	default:
		log.Printf("migration: moved backup allowance %s=%s to %s", oldKey, moved, newKey)
	}

	// The old row goes either way, including when its value was the "0" above.
	// Leaving it would be a second source of truth for one number, and an
	// editable field on the Billing screen that nothing reads is the exact shape
	// that produced this defect.
	if _, err := db.Exec(`DELETE FROM settings WHERE key = $1`, oldKey); err != nil {
		return fmt.Errorf("backup allowance move: delete old key: %w", err)
	}
	return nil
}
