package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/demo"
	"dylaris-core/store"
)

// What a STAFF member - a panel capability, not admin - may no longer do.
// Found by the round-38 audit; the platform has no staff today, so each of
// these is closed before the first one is hired. The plans half moved to
// handlers/settings with the plans handler.

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

type demoLoginStore struct {
	demoStaffStore
}

func (f *demoLoginStore) GetSetting(key string) (string, error) {
	if key == demo.AccountUUIDSetting {
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
