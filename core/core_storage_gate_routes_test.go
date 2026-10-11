package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// coreStorageRoutesFakeStore extends authzFakeStore (routes_authz_test.go)
// with the small amount of ticket + library plumbing needed to drive the READ
// handlers that RequireCoreStorageConfigured must never touch all the way
// through their real business logic, instead of stopping at a recovered
// nil-store panic. Composition, not redeclaration: authzFakeStore itself is
// untouched and reused as-is.
type coreStorageRoutesFakeStore struct {
	*authzFakeStore
	ticket *models.Ticket
}

func (f *coreStorageRoutesFakeStore) GetTicket(id int) (*models.Ticket, error) {
	if f.ticket != nil && f.ticket.ID == id {
		return f.ticket, nil
	}
	return nil, sql.ErrNoRows
}
func (f *coreStorageRoutesFakeStore) IsTicketWatcher(ticketID int, userID string) (bool, error) {
	return false, nil
}
func (f *coreStorageRoutesFakeStore) ListTicketWatchers(ticketID int) ([]models.TicketWatcher, error) {
	return nil, nil
}
func (f *coreStorageRoutesFakeStore) ListTicketAttachments(ticketID int) ([]models.TicketAttachment, error) {
	return nil, nil
}
func (f *coreStorageRoutesFakeStore) GetTicketAttachment(id int) (*models.TicketAttachment, error) {
	return nil, sql.ErrNoRows
}
func (f *coreStorageRoutesFakeStore) ListDisabledLibraryPaths() ([]string, error) {
	return nil, nil
}

// newCoreStorageGateTestServer mirrors newAuthzTestServer (routes_authz_test.go)
// but takes a store.Store directly instead of a concrete *authzFakeStore, so it
// can be driven by coreStorageRoutesFakeStore above. It builds the REAL router
// (buildAPIRouter, the same one main() registers) so this test exercises the
// actual route wiring in routes.go, not a hand-rolled stand-in for it.
func newCoreStorageGateTestServer(t *testing.T, s store.Store) http.Handler {
	t.Helper()
	appState := &handlers.AppState{
		Store:        s,
		FeatureFlags: services.NewFeatureFlags(s),
		Authz:        authz.NewResolver(s),
	}
	authHandler := handlers.NewAuthHandler(appState, testJWTSecret)
	root, _ := buildAPIRouter(appState, authHandler, routeCfg{JWTSecret: testJWTSecret})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		root.ServeHTTP(w, r)
	})
}

// assertCoreStorageGated fails t unless rec is exactly the
// RequireCoreStorageConfigured 503 (feature_disabled / X-Feature-Disabled:
// core_storage). Used as the positive control on the 4 known WRITE routes.
func assertCoreStorageGated(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("X-Feature-Disabled") != "core_storage" {
		t.Errorf("%s: expected the write gate to fire (503, X-Feature-Disabled: core_storage) with no storage configured, got %d X-Feature-Disabled=%q body=%s",
			label, rec.Code, rec.Header().Get("X-Feature-Disabled"), rec.Body.String())
	}
}

// assertNotCoreStorageGated fails t if rec looks like the
// RequireCoreStorageConfigured 503. Any other outcome (200, 403, 404, ...) is
// accepted: the invariant under test is narrowly "the storage gate did not
// block this read", not "the read fully succeeded" - the fake store is
// deliberately minimal, so a route that carries its own unrelated auth/ACL
// logic may still 403/404 for reasons that have nothing to do with storage.
func assertNotCoreStorageGated(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code == http.StatusServiceUnavailable && rec.Header().Get("X-Feature-Disabled") == "core_storage" {
		t.Errorf("%s: BLOCKED by RequireCoreStorageConfigured (503 core_storage) with NO storage configured - reads must never be gated, only writes; body=%s",
			label, rec.Body.String())
	}
}

// TestCoreStorageGate_ReadsNeverBlockedWritesAlwaysBlocked is the permanent
// regression guard for Task 5's central design rule: with NO valid Core file
// storage config, every WRITE that stores a blob (library mkdir/upload,
// ticket attachment upload, ticket backup create) must be gated 503
// core_storage, while every paired READ (list/browse/download) must NOT be -
// existing data stays browsable/downloadable even before storage is
// configured. The implementer verified this once with a throwaway test that
// was deleted rather than committed (see task-5-report.md); this locks it in.
//
// This drives the REAL router (same buildAPIRouter used by main() and by
// routes_authz_test.go/routes_coverage_test.go) rather than asserting on
// middleware structure, so a future edit that accidentally wraps a read route
// in RequireCoreStorageConfigured makes this test fail: the gate is
// unconditional (it runs before any store access), so it would 503
// deterministically regardless of the light fixtures used here.
func TestCoreStorageGate_ReadsNeverBlockedWritesAlwaysBlocked(t *testing.T) {
	fs := &authzFakeStore{settings: map[string]string{
		"feature_tickets_enabled": "true",
		// Deliberately NO core_storage_backend/path/... keys - unconfigured.
	}}
	admin := fs.addUser("admin-id", "root", true)
	wrapped := &coreStorageRoutesFakeStore{
		authzFakeStore: fs,
		ticket:         &models.Ticket{ID: 1, UserID: admin.ID},
	}
	srv := newCoreStorageGateTestServer(t, wrapped)
	id := testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}

	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+mintToken(t, id))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// The 4 routes core/routes.go wraps in RequireCoreStorageConfigured.
	writes := []struct{ method, path string }{
		{"POST", "/api/tickets/1/attachments"},
		{"POST", "/api/admin/tickets/backup"},
		{"POST", "/api/library/mkdir"},
		{"POST", "/api/library/upload"},
	}
	for _, w := range writes {
		assertCoreStorageGated(t, w.method+" "+w.path, do(w.method, w.path))
	}

	// Their read counterparts - must stay reachable with no storage configured.
	reads := []struct{ method, path string }{
		{"GET", "/api/tickets/1/attachments"},
		{"GET", "/api/tickets/1/attachments/1/download"},
		{"GET", "/api/admin/tickets/backups"},
		{"GET", "/api/admin/tickets/backups/nonexistent.json/download"},
		{"GET", "/api/library"},
		{"GET", "/api/library/download?path=/nonexistent.txt"},
	}
	for _, r := range reads {
		assertNotCoreStorageGated(t, r.method+" "+r.path, do(r.method, r.path))
	}
}
