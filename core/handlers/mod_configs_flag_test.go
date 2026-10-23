package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type modConfigsFlagStore struct {
	store.Store
	settings map[string]string
}

func (f *modConfigsFlagStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
// The module rows are not what this test is about; the save logs and moves on.
func (f *modConfigsFlagStore) ListModules() ([]models.Module, error) { return nil, errors.New("no modules here") }
func (f *modConfigsFlagStore) SetSetting(key, value string) error {
	f.settings[key] = value
	return nil
}

// The beta config editor is offered per caller: "admin" must reach admins
// only, and a value the setting cannot hold must not be stored.
func TestModConfigsFlagIsResolvedPerCaller(t *testing.T) {
	st := &modConfigsFlagStore{settings: map[string]string{}}
	state := &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}
	offered := func(admin bool) bool {
		req := httptest.NewRequest("GET", "/api/system/features", nil)
		req = req.WithContext(context.WithValue(req.Context(), "isAdmin", admin))
		rec := httptest.NewRecorder()
		NewSystemFeaturesHandler(state).Get(rec, req)
		var out struct {
			Features map[string]any `json:"features"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Features["modConfigs"] == true
	}
	put := func(v string) int {
		rec := httptest.NewRecorder()
		NewFeatureSettingsHandler(state).Set(rec, httptest.NewRequest("PUT", "/api/admin/settings/features", strings.NewReader(`{"modConfigs":"`+v+`"}`)))
		return rec.Code
	}

	if offered(true) || offered(false) {
		t.Fatal("offered while the setting was never saved; the default is off")
	}
	if code := put("admin"); code != 200 {
		t.Fatalf("PUT admin answered %d", code)
	}
	if !offered(true) || offered(false) {
		t.Fatalf("admin setting: admin=%v member=%v, want only the admin", offered(true), offered(false))
	}
	put("all")
	if !offered(false) {
		t.Fatal("all setting: a member is not offered the editor")
	}
	if code := put("everyone"); code != 400 {
		t.Fatalf("an unknown audience answered %d, want 400", code)
	}
	if st.settings[services.ModConfigsSettingKey] != "all" {
		t.Fatalf("stored %q after a refused PUT, want all kept", st.settings[services.ModConfigsSettingKey])
	}
}
