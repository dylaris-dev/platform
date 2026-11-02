package handlers

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type templatesFlagStore struct {
	store.Store
	settings map[string]string
	module   models.Module
}

func (f *templatesFlagStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
func (f *templatesFlagStore) SetSetting(key, value string) error {
	f.settings[key] = value
	return nil
}

// Connection 4 exists; any other id is missing, as the real store answers it.
func (f *templatesFlagStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	if id != 4 {
		return nil, sql.ErrNoRows
	}
	return &models.StorageConnection{ID: id}, nil
}
func (f *templatesFlagStore) ListModules() ([]models.Module, error) {
	return []models.Module{f.module}, nil
}

// The Modpacks row is absent here, so the save tries to create it.
func (f *templatesFlagStore) CreateModule(*models.Module) (int, error) { return 0, nil }
func (f *templatesFlagStore) UpdateModuleStatus(_ int, on bool) error {
	f.module.IsEnabled = on
	return nil
}
func (f *templatesFlagStore) SetModuleAccessRole(_ int, role string) error {
	f.module.AccessRole = role
	return nil
}

// The seven settings are validated as a whole, saved partially, and the module
// row follows the audience.
func TestTemplatesFeatureSettingsPut(t *testing.T) {
	st := &templatesFlagStore{
		settings: map[string]string{},
		module:   models.Module{ID: 9, Name: "Templates", AccessRole: "admin"},
	}
	state := withCoreStorage(&AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)})
	put := func(body string) int {
		rec := httptest.NewRecorder()
		NewFeatureSettingsHandler(state).Set(rec, httptest.NewRequest("PUT", "/api/admin/settings/features", strings.NewReader(body)))
		return rec.Code
	}

	for _, bad := range []string{
		`{"templates":"everyone"}`, `{"templatesMaxPerUser":"-1"}`, `{"templatesMaxSizeMb":"lots"}`,
		`{"templatesContentPolicy":"anything"}`, `{"templatesStorageConnectionId":-2}`,
		`{"templates":"all","templatesContentPolicy":"anything"}`,
		`{"templatesStorageConnectionId":7}`,
		`{"templatesMaxSizeMb":"1048577"}`, `{"templatesMaxSizeMb":"9223372036854775807"}`,
		`{"templatesMaxSizeMbAdmin":"-1"}`, `{"templatesMaxSizeMbAdmin":"1048577"}`,
	} {
		if code := put(bad); code != 400 {
			t.Errorf("%s answered %d, want 400", bad, code)
		}
	}
	if len(st.settings) != 0 {
		t.Fatalf("refused PUTs stored %v", st.settings)
	}

	if code := put(`{"templates":"admin","templatesMaxPerUser":"unlimited","templatesMaxSizeMb":"0","templatesContentPolicy":"uploads","templatesPlatformForUsers":true,"templatesStorageConnectionId":4}`); code != 200 {
		t.Fatalf("PUT answered %d", code)
	}
	ts := services.LoadTemplateSettings(context.Background(), state.FeatureFlags)
	if ts.Audience != "admin" || ts.MaxPerUser != nil || ts.MaxSizeBytes == nil || *ts.MaxSizeBytes != 0 ||
		ts.ContentPolicy != "uploads" || !ts.PlatformForUsers || ts.CreateRequiresRole || ts.StorageConnectionID != 4 {
		t.Fatalf("settings after PUT: %+v", ts)
	}
	if !st.module.IsEnabled || st.module.AccessRole != "admin" {
		t.Fatalf("module after admin: %+v", st.module)
	}

	if code := put(`{"templatesMaxSizeMbAdmin":"4096"}`); code != 200 {
		t.Fatalf("admin size answered %d, want 200", code)
	}
	if ts := services.LoadTemplateSettings(context.Background(), state.FeatureFlags); ts.MaxSizeBytesAdmin == nil || *ts.MaxSizeBytesAdmin != 4096<<20 || *ts.MaxSizeBytes != 0 {
		t.Fatalf("admin size read back as %v (user %v)", ts.MaxSizeBytesAdmin, ts.MaxSizeBytes)
	}

	if code := put(`{"templatesMaxSizeMb":"1048576"}`); code != 200 {
		t.Fatalf("1 TiB answered %d, want 200", code)
	}
	if ts := services.LoadTemplateSettings(context.Background(), state.FeatureFlags); ts.MaxSizeBytes == nil || *ts.MaxSizeBytes != 1<<40 {
		t.Fatalf("1 TiB read back as %v", ts.MaxSizeBytes)
	}

	// Partial: only the audience moves, the rest is kept.
	put(`{"templates":"all"}`)
	if ts := services.LoadTemplateSettings(context.Background(), state.FeatureFlags); ts.Audience != "all" || ts.ContentPolicy != "uploads" {
		t.Fatalf("partial PUT lost settings: %+v", ts)
	}
	if st.module.AccessRole != "all" {
		t.Fatalf("module role after all: %q", st.module.AccessRole)
	}
	put(`{"templates":"off","templatesStorageConnectionId":0}`)
	if st.module.IsEnabled || st.settings[services.TemplatesStorageConnectionKey] != "" {
		t.Fatalf("off: module=%+v storage=%q", st.module, st.settings[services.TemplatesStorageConnectionKey])
	}
}
