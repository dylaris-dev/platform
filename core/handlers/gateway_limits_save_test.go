package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/store"
)

// gatewayLimitsFakeStore records what the save did with each scope. The embedded
// nil store.Store makes any unexpected call panic rather than pass quietly.
type gatewayLimitsFakeStore struct {
	store.Store

	set      map[string]*int
	deleted  map[string]bool
	settings map[string]string
}

func newGatewayLimitsFakeStore() *gatewayLimitsFakeStore {
	return &gatewayLimitsFakeStore{set: map[string]*int{}, deleted: map[string]bool{}, settings: map[string]string{}}
}

func (f *gatewayLimitsFakeStore) SetSetting(k, v string) error {
	f.settings[k] = v
	return nil
}
func (f *gatewayLimitsFakeStore) SetGatewayRouteLimit(scope string, max *int) error {
	f.set[scope] = max
	return nil
}
func (f *gatewayLimitsFakeStore) DeleteGatewayRouteLimit(scope string) error {
	f.deleted[scope] = true
	return nil
}

func saveGatewayLimits(t *testing.T, body string) *gatewayLimitsFakeStore {
	t.Helper()
	fs := newGatewayLimitsFakeStore()
	h := &SettingsHandler{state: &AppState{Store: fs}}
	r := httptest.NewRequest(http.MethodPost, "/api/admin/settings/gateway", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.SaveGatewaySettings(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", w.Code, w.Body.String())
	}
	return fs
}

// A blank limit must DELETE its row, not store NULL.
//
// effectiveRouteLimit walks user:<id> -> user_default -> global, and a row that
// exists answers even when its value is NULL, which ends the walk. Saving this
// page with the per-user default left blank therefore wrote a NULL user_default
// row and global stopped being asked: every tenant uncapped, the global number
// still displayed on the page, and nothing failing anywhere. This is what the
// live instance was found in, with all four rows NULLed.
func TestABlankRouteLimitDeletesItsRowInsteadOfStoringNull(t *testing.T) {
	fs := saveGatewayLimits(t, `{"limits":{"global":5,"userDefault":null}}`)

	if !fs.deleted["user_default"] {
		t.Error("a blank per-user default left a row behind; that row answers the scope walk with \"no cap\" and the global limit below it is never asked")
	}
	if v, ok := fs.set["user_default"]; ok && v == nil {
		t.Error("a blank per-user default was stored as NULL, which reads as \"set, and set to no cap\"")
	}
	if fs.deleted["global"] {
		t.Error("the global limit was 5 and must be written, not deleted")
	}
	if v := fs.set["global"]; v == nil || *v != 5 {
		t.Errorf("global = %v, want 5", v)
	}
}

// The other half of the convention: 0 is a real cap meaning "none", and must
// survive the save as 0. It is the one number an operator can enter to stop an
// account from holding any address at all, and on this platform that number has
// twice been the one that switched the check off instead.
func TestARouteLimitOfZeroIsSavedAsZero(t *testing.T) {
	fs := saveGatewayLimits(t, `{"limits":{"global":0,"userDefault":0}}`)

	for _, scope := range []string{"global", "user_default"} {
		if fs.deleted[scope] {
			t.Errorf("%s: a cap of 0 was deleted, which turns \"none\" into \"unlimited\"", scope)
		}
		v, ok := fs.set[scope]
		if !ok || v == nil || *v != 0 {
			t.Errorf("%s = %v, want 0", scope, v)
		}
	}
}

// null is "no cap" for the custom-domain limit, so a request WITHOUT the field
// must not be read as null: a client that predates it would lift the cap every
// time it saved this screen.
func TestTheCustomDomainCapSurvivesAClientThatDoesNotSendIt(t *testing.T) {
	if fs := saveGatewayLimits(t, `{"limits":{"global":5}}`); fs.settings[SettingMaxCustomDomains] != "" {
		t.Errorf("an absent field wrote %q", fs.settings[SettingMaxCustomDomains])
	}
	if fs := saveGatewayLimits(t, `{"limits":{"global":5},"maxCustomDomains":null}`); fs.settings[SettingMaxCustomDomains] != "unlimited" {
		t.Errorf("an explicit null wrote %q, want unlimited", fs.settings[SettingMaxCustomDomains])
	}
	if fs := saveGatewayLimits(t, `{"limits":{"global":5},"maxCustomDomains":7}`); fs.settings[SettingMaxCustomDomains] != "7" {
		t.Errorf("a cap of 7 wrote %q", fs.settings[SettingMaxCustomDomains])
	}
}
