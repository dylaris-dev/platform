package tickets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
)

// Every handler in ticket_migration.go must refuse a non-admin.
//
// The file's own doc says "All operations are admin-only" and nothing enforced
// it: the routes gate on tickets.read / tickets.write, ordinary panel
// capabilities an admin can grant to any role and labelled "View tickets" and
// "Edit tickets" in the catalog. tickets.read reached DownloadBackup - a JSON
// dump of every ticket table, past every per-ticket and per-team visibility
// check - and tickets.write reached the migration to an arbitrary DSN and the
// restore that wipes every ticket table.
//
// The AppState here carries a nil Store on purpose. Any handler that gets past
// the gate touches it and panics, so "no panic and a 403" is the assertion:
// the gate fired before anything else ran.
func TestEveryTicketMigrationHandlerRefusesANonAdmin(t *testing.T) {
	h := NewTicketMigrationHandler(&handlers.AppState{})

	handlers := map[string]http.HandlerFunc{
		"GetStatus":              h.GetStatus,
		"TestExternalConnection": h.TestExternalConnection,
		"DryRunMigration":        h.DryRunMigration,
		"ExecuteMigration":       h.ExecuteMigration,
		"CreateBackup":           h.CreateBackup,
		"ListBackups":            h.ListBackups,
		"DownloadBackup":         h.DownloadBackup,
		"DeleteBackup":           h.DeleteBackup,
		"InitRestore":            h.InitRestore,
		"ExecuteRestore":         h.ExecuteRestore,
	}

	for name, fn := range handlers {
		t.Run(name, func(t *testing.T) {
			for _, caller := range []struct {
				label string
				ctx   func(*http.Request) *http.Request
			}{
				{"a capability holder who is not an admin", func(r *http.Request) *http.Request {
					return r.WithContext(context.WithValue(r.Context(), "isAdmin", false))
				}},
				{"a caller with no admin flag at all", func(r *http.Request) *http.Request { return r }},
			} {
				r := httptest.NewRequest(http.MethodPost, "/api/admin/tickets/x", strings.NewReader("{}"))
				r = caller.ctx(r)
				r = r.WithContext(context.WithValue(r.Context(), "userID", "11111111-1111-4111-8111-111111111111"))
				w := httptest.NewRecorder()

				fn(w, r)

				if w.Code != http.StatusForbidden {
					t.Errorf("%s: status = %d, want 403", caller.label, w.Code)
				}
			}
		})
	}
}

// And the gate must not be the only thing standing there: an admin still gets
// through it. Proven by the panic a nil Store causes once the handler proceeds.
func TestTheAdminGateStillLetsAnAdminThrough(t *testing.T) {
	h := NewTicketMigrationHandler(&handlers.AppState{})
	r := httptest.NewRequest(http.MethodGet, "/api/admin/tickets/migration/status", nil)
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
	w := httptest.NewRecorder()

	defer func() {
		if recover() == nil {
			t.Error("an admin was stopped by the gate; GetStatus never reached the store")
		}
	}()
	h.GetStatus(w, r)
}
