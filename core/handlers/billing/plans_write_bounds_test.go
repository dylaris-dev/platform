package billing

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
)

// plans.write is not an admin capability. Its holder granted themselves BYON
// and unlimited limits, or lifted their own suspension, on four routes where
// only the billing status asked whose account it was.
func TestPlansWriteCannotBeSpentOnTheCallersOwnAccountBilling(t *testing.T) {
	calls := map[string]func(s *handlers.AppState, w http.ResponseWriter, r *http.Request){
		"entitlement grant": func(s *handlers.AppState, w http.ResponseWriter, r *http.Request) {
			(&EntitlementHandler{state: s}).Grant(w, r)
		},
		"entitlement revoke": func(s *handlers.AppState, w http.ResponseWriter, r *http.Request) {
			(&EntitlementHandler{state: s}).Revoke(w, r)
		},
		"billing overrides": func(s *handlers.AppState, w http.ResponseWriter, r *http.Request) {
			(&BillingHandler{state: s}).SetBillingOverrides(w, r)
		},
	}
	for name, call := range calls {
		t.Run(name+"/own account", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: staffID}}
			s := &handlers.AppState{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, staffRequest("POST", map[string]string{"id": staffID}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on the caller's own account", rw.Code)
			}
		})
		t.Run(name+"/an admin", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: visAdmin, IsAdmin: true}}
			s := &handlers.AppState{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, staffRequest("POST", map[string]string{"id": visAdmin}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on an admin's account", rw.Code)
			}
		})
	}
}
