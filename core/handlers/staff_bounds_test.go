package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// What a STAFF member - a panel capability, not admin - may no longer do.
// Found by the round-38 audit; the platform has no staff today, so each of
// these is closed before the first one is hired.

const staffID = "33333333-3333-3333-3333-333333333333"

func staffRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", staffID)
	ctx = context.WithValue(ctx, "username", "staff")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

type demoStaffStore struct {
	store.Store
	user  *models.User
	roleC []string
}

func (f *demoStaffStore) GetUserByUsername(string) (*models.User, error) { return f.user, nil }
func (f *demoStaffStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	if f.roleC == nil {
		return nil, store.CapOverrides{}, nil
	}
	id := 1
	return &id, store.CapOverrides{}, nil
}
func (f *demoStaffStore) GetPanelRole(int) (*store.PanelRole, error) {
	return &store.PanelRole{Capabilities: f.roleC}, nil
}
func (f *demoStaffStore) SetSetting(string, string) error { return nil }

type termsStore struct {
	store.Store
	target *models.User
}

func (f *termsStore) GetUserByID(string) (*models.User, error) { return f.target, nil }
func (f *termsStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{Grant: []string{"plans.write"}}, nil
}
func (f *termsStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

type demoLoginStore struct {
	demoStaffStore
}

func (f *demoLoginStore) GetSetting(key string) (string, error) {
	if key == demoAccountUUIDSetting {
		return "u-demo", nil
	}
	return "", nil
}
func (f *demoLoginStore) GetUserByID(string) (*models.User, error) { return f.user, nil }

// The account can be given a staff role after it was designated, and every
// public demo session would then hold it.
func TestADemoLoginRefusesAnAccountThatGainedStaffRights(t *testing.T) {
	st := &demoLoginStore{demoStaffStore{user: &models.User{ID: "u-demo", Username: "demo"}, roleC: []string{"users.read"}}}
	rw := httptest.NewRecorder()
	(&AuthHandler{state: &AppState{Store: st, StoreEnabled: true}, jwtKey: []byte("k")}).DemoLogin(rw, httptest.NewRequest("POST", "/x", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("a demo session was issued for a staff account: status %d", rw.Code)
	}
}

// plans.write is not an admin capability. Its holder granted themselves BYON
// and unlimited limits, or lifted their own suspension, on four routes where
// only the billing status asked whose account it was.
func TestPlansWriteCannotBeSpentOnTheCallersOwnAccount(t *testing.T) {
	calls := map[string]func(s *AppState, w http.ResponseWriter, r *http.Request){
		"limit overrides": func(s *AppState, w http.ResponseWriter, r *http.Request) {
			(&PlansHandler{state: s}).SetUserLimitOverrides(w, r)
		},
	}
	for name, call := range calls {
		t.Run(name+"/own account", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: staffID}}
			s := &AppState{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, staffRequest("POST", map[string]string{"id": staffID}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on the caller's own account", rw.Code)
			}
		})
		t.Run(name+"/an admin", func(t *testing.T) {
			st := &termsStore{target: &models.User{ID: visAdmin, IsAdmin: true}}
			s := &AppState{Store: st, Authz: authz.NewResolver(st)}
			rw := httptest.NewRecorder()
			call(s, rw, staffRequest("POST", map[string]string{"id": visAdmin}, `{"kind":"byon","days":30}`))
			if rw.Code != http.StatusForbidden {
				t.Fatalf("status %d on an admin's account", rw.Code)
			}
		})
	}
}
