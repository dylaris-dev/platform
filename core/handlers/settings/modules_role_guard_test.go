package settings

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

// modulesRoleGuardFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only GetModuleByID/SetModuleAccessRole are
// overridden. Mirrors modulesDeleteGuardFakeStore in modules_delete_guard_test.go.
type modulesRoleGuardFakeStore struct {
	store.Store
	mod          *models.Module
	setRoleCalls int
}

func (f *modulesRoleGuardFakeStore) GetModuleByID(id int) (*models.Module, error) {
	return f.mod, nil
}

func (f *modulesRoleGuardFakeStore) SetModuleAccessRole(id int, role string) error {
	f.setRoleCalls++
	return nil
}

func moduleRoleReq(id, role string) *http.Request {
	body := bytes.NewBufferString(`{"role":"` + role + `"}`)
	r := httptest.NewRequest(http.MethodPatch, "/api/modules/"+id+"/role", body)
	r = mux.SetURLVars(r, map[string]string{"id": id})
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
}

// TestSetModuleAccessRole_ProtectsAdminOnlyBuiltins asserts the PATCH
// /modules/{id}/role guard: the always-visible Servers module and the
// admin-only Admin and Infrastructure modules reject a role change, while a
// role-configurable module (e.g. Tickets) reaches the store. Infrastructure
// must be rejected so a direct settings.write call cannot flip it to "all" and
// leave non-admins a dead nav link (the panel already front-locks it).
func TestSetModuleAccessRole_ProtectsAdminOnlyBuiltins(t *testing.T) {
	tests := []struct {
		name        string
		mod         *models.Module
		role        string
		wantStatus  int
		wantSetCall bool
	}{
		{
			name:        "Servers rejects a role change (always all)",
			mod:         &models.Module{ID: 1, Name: "Servers"},
			role:        "admin",
			wantStatus:  http.StatusBadRequest,
			wantSetCall: false,
		},
		{
			name:        "Admin rejects a role change (always admin-only)",
			mod:         &models.Module{ID: 2, Name: "Admin"},
			role:        "all",
			wantStatus:  http.StatusBadRequest,
			wantSetCall: false,
		},
		{
			name:        "Infrastructure rejects a role change (always admin-only)",
			mod:         &models.Module{ID: 3, Name: "Infrastructure"},
			role:        "all",
			wantStatus:  http.StatusBadRequest,
			wantSetCall: false,
		},
		{
			name:        "Tickets accepts a role change",
			mod:         &models.Module{ID: 4, Name: "Tickets"},
			role:        "all",
			wantStatus:  http.StatusOK,
			wantSetCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &modulesRoleGuardFakeStore{mod: tt.mod}
			h := NewModuleHandler(Deps{Store: fs})
			rec := httptest.NewRecorder()

			h.SetModuleAccessRoleHandler(rec, moduleRoleReq("1", tt.role))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantSetCall && fs.setRoleCalls != 1 {
				t.Fatalf("expected exactly 1 SetModuleAccessRole call, got %d", fs.setRoleCalls)
			}
			if !tt.wantSetCall && fs.setRoleCalls != 0 {
				t.Fatalf("expected no SetModuleAccessRole call, got %d", fs.setRoleCalls)
			}
		})
	}
}
