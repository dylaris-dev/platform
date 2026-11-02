package settings

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/store"
)

// What a STAFF member - a panel capability, not admin - may no longer do.
// Found by the round-38 audit; the platform has no staff today, so each of
// these is closed before the first one is hired.

type termsStore struct {
	store.Store
	target *models.User
}

func (f *termsStore) GetUserByID(string) (*models.User, error) { return f.target, nil }
func (f *termsStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{Grant: []string{"plans.write"}}, nil
}
func (f *termsStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

// plans.write is not an admin capability. Its holder granted themselves BYON
// and unlimited limits, or lifted their own suspension, on four routes where
// only the billing status asked whose account it was.
func TestPlansWriteCannotBeSpentOnTheCallersOwnAccount(t *testing.T) {
	calls := map[string]func(s Deps, w http.ResponseWriter, r *http.Request){
		"limit overrides": func(s Deps, w http.ResponseWriter, r *http.Request) {
			(&PlansHandler{d: s}).SetUserLimitOverrides(w, r)
		},
	}
	for name, call := range calls {
		t.Run(name+"/own account", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: handlertest.StaffID}}
			s := Deps{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, handlertest.StaffRequest("POST", map[string]string{"id": handlertest.StaffID}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on the caller's own account", rw.Code)
			}
		})
		t.Run(name+"/an admin", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: handlertest.VisAdmin, IsAdmin: true}}
			s := Deps{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, handlertest.StaffRequest("POST", map[string]string{"id": handlertest.VisAdmin}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on an admin's account", rw.Code)
			}
		})
	}
}
