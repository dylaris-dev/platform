package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

// modulesDeleteGuardFakeStore embeds store.Store (nil) so it satisfies the
// full interface at compile time; only GetModuleByID/DeleteModule are
// overridden. Mirrors the panelRolesFakeStore pattern in panel_roles_test.go.
type modulesDeleteGuardFakeStore struct {
	store.Store
	mod         *models.Module
	deleteCalls int
}

func (f *modulesDeleteGuardFakeStore) GetModuleByID(id int) (*models.Module, error) {
	return f.mod, nil
}

func (f *modulesDeleteGuardFakeStore) DeleteModule(id int) error {
	f.deleteCalls++
	return nil
}

func moduleDeleteReq(id string) *http.Request {
	r := httptest.NewRequest(http.MethodDelete, "/api/modules/"+id, nil)
	r = mux.SetURLVars(r, map[string]string{"id": id})
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
}

// TestDeleteModule_ProtectsBuiltInsRejectsCustom asserts the DELETE
// /modules/{id} guard: built-in/system modules (matched by name via
// builtInModules, or by is_system) are rejected, while a genuinely custom
// module reaches the store delete. Tickets is a system module in spirit
// (seeded by Core, re-created on restart) but is deliberately kept
// is_system=false (see database/db_tables.go seedSystemModules) so it stays
// toggle-able; its delete protection must therefore come from the same
// name-based builtInModules guard, not from IsSystem.
func TestDeleteModule_ProtectsBuiltInsRejectsCustom(t *testing.T) {
	tests := []struct {
		name           string
		mod            *models.Module
		wantStatus     int
		wantDeleteCall bool
	}{
		{
			name:           "Tickets is protected by name even though is_system=false",
			mod:            &models.Module{ID: 5, Name: "Tickets", IsSystem: false},
			wantStatus:     http.StatusBadRequest,
			wantDeleteCall: false,
		},
		{
			name:           "Servers is protected via is_system (existing guard)",
			mod:            &models.Module{ID: 1, Name: "Servers", IsSystem: true},
			wantStatus:     http.StatusBadRequest,
			wantDeleteCall: false,
		},
		{
			name:           "a genuinely custom module is deletable",
			mod:            &models.Module{ID: 42, Name: "MyCustomIframe", IsSystem: false},
			wantStatus:     http.StatusOK,
			wantDeleteCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &modulesDeleteGuardFakeStore{mod: tt.mod}
			h := NewModuleHandler(&AppState{Store: fs})
			rec := httptest.NewRecorder()

			h.DeleteModuleHandler(rec, moduleDeleteReq("1"))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if fs.deleteCalls != 0 && !tt.wantDeleteCall {
				t.Fatalf("expected no DeleteModule call, got %d", fs.deleteCalls)
			}
			if tt.wantDeleteCall && fs.deleteCalls != 1 {
				t.Fatalf("expected exactly 1 DeleteModule call, got %d", fs.deleteCalls)
			}
		})
	}
}
