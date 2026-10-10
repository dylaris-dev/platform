package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// The memory guard columns: added idempotently, defaulted to 'off' for every
// existing row, and read back by both the single-row and the list queries the
// panel and the guard consumer use.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationMemoryGuardColumns(t *testing.T) {
	db, st := integrationDB(t)
	// InitDB already applied it once; a second boot must be a no-op.
	if err := applyMemoryGuardSchema(db); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	f := newFixture(t, st)

	got, err := st.GetServerByID(f.server.ID)
	if err != nil {
		t.Fatalf("GetServerByID: %v", err)
	}
	if got.MemoryGuardAction != models.MemoryGuardOff || got.LastCrashReason != nil || got.LastCrashAt != nil {
		t.Fatalf("fresh server: action %q crash %v at %v, want off and no crash", got.MemoryGuardAction, got.LastCrashReason, got.LastCrashAt)
	}

	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if err := st.SetServerMemoryGuardAction(f.server.ID, models.MemoryGuardRestart); err != nil {
		t.Fatalf("SetServerMemoryGuardAction: %v", err)
	}
	if err := st.SetServerLastCrash(f.server.ID, models.CrashReasonOOMKilled, at); err != nil {
		t.Fatalf("SetServerLastCrash: %v", err)
	}

	byUUID, err := st.GetServerByUUID(f.server.UUID)
	if err != nil {
		t.Fatalf("GetServerByUUID: %v", err)
	}
	list, err := st.ListServersForUser(f.user.ID, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListServersForUser: %v (%d rows)", err, len(list))
	}
	for name, s := range map[string]*models.Server{"by uuid": byUUID, "list": &list[0]} {
		if s.MemoryGuardAction != models.MemoryGuardRestart {
			t.Errorf("%s: action %q, want restart", name, s.MemoryGuardAction)
		}
		if s.LastCrashReason == nil || *s.LastCrashReason != models.CrashReasonOOMKilled || s.LastCrashAt == nil || !s.LastCrashAt.Equal(at) {
			t.Errorf("%s: crash %v at %v, want oom_killed at %v", name, s.LastCrashReason, s.LastCrashAt, at)
		}
	}
}
