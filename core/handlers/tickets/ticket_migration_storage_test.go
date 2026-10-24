package tickets

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/services/storagereach"
)

// TestRequireCoreStorageReachable_GatesListBackups closes the gap Fix 2 in
// the final review wave found: ListBackups builds a core storage provider
// (ticket_migration.go) just like the eight routes routes.go already wrapped,
// but was missing the wrapper itself - on a fake-shared volume the mount is
// healthy, ListFiles succeeds against an empty directory, and the endpoint
// answers 200 with an empty array, which reads as "no backups exist" rather
// than "this Core cannot see them". Runs the actual production handler
// method, not a fake closure, so a regression that drops the wrapper in
// routes.go without also touching this test would still be caught only if
// routes.go used a differently-shaped handler - which is exactly what this
// test would fail to compile against.
func TestRequireCoreStorageReachable_GatesListBackups(t *testing.T) {
	svc := storagereach.NewService(storagereach.ServiceDeps{CoreID: "core-a"})
	svc.Status().Set(storagereach.StatusUnreachable, "/mnt/nfs/dylaris-shared: no such file or directory")
	s := &handlers.AppState{StorageReach: svc}
	h := NewTicketMigrationHandler(s)

	gated := s.RequireCoreStorageReachable(h.ListBackups)
	rec := httptest.NewRecorder()
	gated(rec, httptest.NewRequest("GET", "/api/admin/tickets/backups", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; ListBackups ran on a Core that cannot reach the shared storage", rec.Code)
	}
}
