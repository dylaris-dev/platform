package database

import (
	"testing"

	"dylaris-core/store"
)

// runtime_changed rows written before the handler stopped storing JVM flags
// lose the value at boot; other events and other keys are left alone.
func TestIntegrationBootScrubsJvmFlagsFromAuditRows(t *testing.T) {
	db := freshSchemaDB(t)
	f := newFixture(t, store.NewPostgresStore(db))
	insert := func(event, meta string) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(`INSERT INTO server_audit_events (server_id, event_type, metadata)
			VALUES ($1, $2, $3::jsonb) RETURNING id`, f.server.ID, event, meta).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := insert("runtime_changed", `{"java_image":"j21","jvm_flags":"-Dtoken=s3cret"}`)
	other := insert("resources_changed", `{"jvm_flags":"untouched"}`)

	for i := 0; i < 2; i++ {
		if err := applyAuditJvmFlagsScrub(db); err != nil {
			t.Fatalf("scrub run %d: %v", i+1, err)
		}
	}
	read := func(id int64) string {
		t.Helper()
		var m string
		if err := db.QueryRow(`SELECT metadata::text FROM server_audit_events WHERE id = $1`, id).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if got := read(old); got != `{"java_image": "j21"}` {
		t.Errorf("runtime_changed row after scrub: %s", got)
	}
	if got := read(other); got != `{"jvm_flags": "untouched"}` {
		t.Errorf("another event was touched: %s", got)
	}
}
