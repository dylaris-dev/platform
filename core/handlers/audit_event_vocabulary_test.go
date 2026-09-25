package handlers

import (
	"reflect"
	"testing"
)

// maintenance_toggled was five things at once: real maintenance mode, starting
// a database migration, converting the statistics table, starting or
// cancelling a storage migration, and deleting a storage manifest. The actual
// action lived in a metadata field, so the identity log - which now has a
// screen - answered "somebody moved the entire platform database to another
// host" with "Maintenance mode changed".
//
// This pins the vocabulary rather than the call sites: a constant is cheap to
// reuse by accident, and the failure is invisible until somebody reads the log
// after an incident.
func TestTheDataMovingEventsHaveTheirOwnNames(t *testing.T) {
	names := map[string]string{
		"maintenance":           AuditEventMaintenanceToggled,
		"db migration":          AuditEventDBMigrationStarted,
		"hypertable conversion": AuditEventDBHypertableConverted,
		"storage migration":     AuditEventStorageMigrationStart,
		"storage cancel":        AuditEventStorageMigrationCancel,
		"manifest delete":       AuditEventStorageManifestDeleted,
	}
	seen := map[string]string{}
	for what, id := range names {
		if id == "" {
			t.Errorf("%s has no event id", what)
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("%s and %s share the event id %q; the log cannot tell them apart", prev, what, id)
		}
		seen[id] = what
	}
	if len(seen) != len(names) {
		t.Fatalf("got %d distinct ids for %d actions: %v", len(seen), len(names), reflect.ValueOf(seen).MapKeys())
	}
	// The one that keeps the old id is the one it was always named after, so
	// rows written before the split still read correctly.
	if AuditEventMaintenanceToggled != "maintenance_toggled" {
		t.Errorf("maintenance changed id to %q; the rows already written would become unreadable",
			AuditEventMaintenanceToggled)
	}
}
