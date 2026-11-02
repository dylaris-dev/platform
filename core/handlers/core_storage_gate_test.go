package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"
)

// bulkModpackCall records one BulkSetCanCreateModpacks invocation.
type bulkModpackCall struct {
	can           bool
	includeManual bool
}

// gateFakeStore serves GetSetting from a map; embeds store.Store for the rest.
type gateFakeStore struct {
	store.Store
	kv        map[string]string
	modules   []models.Module
	bulkCalls []bulkModpackCall
}

// The features GET counts enabled ticket categories, so this fake has to answer
// it - the embedded nil store.Store panics otherwise. None here, which is the
// state that makes the screen warn.
func (f *gateFakeStore) ListTicketCategories(bool) ([]models.TicketCategory, error) {
	return nil, nil
}

func (f *gateFakeStore) GetSetting(key string) (string, error) { return f.kv[key], nil }

// SetSetting persists into the same map GetSetting reads from. Needed once a
// test drives FeatureSettingsHandler.Set() past its guards into the actual
// writes loop (e.g. the tickets:false success path), which calls SetSetting
// for every toggle in the bundle.
func (f *gateFakeStore) SetSetting(key, value string) error {
	if f.kv == nil {
		f.kv = map[string]string{}
	}
	f.kv[key] = value
	return nil
}

// The module surface below exists because FeatureSettingsHandler.Set keeps the
// Modpacks navbar row in step with the two modpack flags. Recorded rather than
// ignored, so a test can assert on the derived row.
func (f *gateFakeStore) ListModules() ([]models.Module, error) { return f.modules, nil }

func (f *gateFakeStore) CreateModule(m *models.Module) (int, error) {
	m.ID = len(f.modules) + 1
	f.modules = append(f.modules, *m)
	return m.ID, nil
}

func (f *gateFakeStore) UpdateModuleStatus(id int, isEnabled bool) error {
	for i := range f.modules {
		if f.modules[i].ID == id {
			f.modules[i].IsEnabled = isEnabled
		}
	}
	return nil
}

func (f *gateFakeStore) SetModuleAccessRole(id int, role string) error {
	for i := range f.modules {
		if f.modules[i].ID == id {
			f.modules[i].AccessRole = role
		}
	}
	return nil
}

// BulkSetCanCreateModpacks records the call so a test can assert what the
// authoring toggle asked for, including whether manual rows were included.
func (f *gateFakeStore) BulkSetCanCreateModpacks(can bool, includeManual bool) (int64, error) {
	f.bulkCalls = append(f.bulkCalls, bulkModpackCall{can: can, includeManual: includeManual})
	return int64(len(f.bulkCalls)), nil
}

func TestFeatureSettings_RefusesTicketsWhenStorageUnconfigured(t *testing.T) {
	newHandler := func() *FeatureSettingsHandler {
		st := &AppState{
			Store:  &gateFakeStore{kv: map[string]string{}},
			Events: services.NewSystemEventsPublisher(nil),
		}
		st.CoreStorage = &corestorage.Manager{Store: st.Store, Events: st.Events}
		st.FeatureFlags = services.NewFeatureFlags(st.Store)
		return NewFeatureSettingsHandler(st)
	}

	t.Run("tickets true is refused when storage unconfigured", func(t *testing.T) {
		h := newHandler()
		rw := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/features", strings.NewReader(`{"tickets":true,"modpacks":true,"autoMove":false}`))
		h.Set(rw, req)
		if rw.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 when enabling tickets without storage (%s)", rw.Code, rw.Body.String())
		}
	})

	// The other half of the ON/OFF asymmetry: an operator must never be
	// trapped in an enabled-but-broken state, so turning Tickets OFF (or
	// leaving it off) must always succeed regardless of storage config.
	t.Run("tickets false always succeeds when storage unconfigured", func(t *testing.T) {
		h := newHandler()
		rw := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/features", strings.NewReader(`{"tickets":false,"modpacks":false,"autoMove":false}`))
		h.Set(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: disabling tickets must never be blocked by the storage gate (%s)", rw.Code, rw.Body.String())
		}
	})
}
