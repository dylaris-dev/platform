package database

import (
	"database/sql"
	"fmt"
)

// applyAuditJvmFlagsScrub removes the JVM flags that runtime_changed audit rows
// used to store in full. A flag can carry a secret (-Dsome.token=...), and the
// audit trail kept every old value for as long as the retention allows, which
// by default is forever. The handler now records only whether the flags
// changed; this strips the value from rows written before that.
//
// Idempotent: a scrubbed row no longer has the key, so a later boot matches
// nothing.
func applyAuditJvmFlagsScrub(db *sql.DB) error {
	if _, err := db.Exec(`UPDATE server_audit_events
		SET metadata = metadata - 'jvm_flags'
		WHERE event_type = 'runtime_changed' AND metadata ? 'jvm_flags'`); err != nil {
		return fmt.Errorf("audit jvm flags scrub: %w", err)
	}
	return nil
}
