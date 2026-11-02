package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// newFeatureSettingsFixture wires FeatureSettingsHandler over gateFakeStore with
// a live FeatureFlags, so a flag written by one request is visible to the next.
func newFeatureSettingsFixture(t *testing.T, seed map[string]string) (*FeatureSettingsHandler, *gateFakeStore) {
	t.Helper()
	fake := &gateFakeStore{kv: map[string]string{}}
	for k, v := range seed {
		fake.kv[k] = v
	}
	st := withCoreStorage(&AppState{StoreEnabled: true, Store: fake})
	st.FeatureFlags = services.NewFeatureFlags(fake)
	st.Events = services.NewSystemEventsPublisher(nil)
	return NewFeatureSettingsHandler(st), fake
}

func putFeatures(t *testing.T, h *FeatureSettingsHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/features", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Set(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT features status = %d, body %s", rec.Code, rec.Body.String())
	}
	return rec
}

func findModule(t *testing.T, fake *gateFakeStore, name string) models.Module {
	t.Helper()
	for _, m := range fake.modules {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no %q module row; have %+v", name, fake.modules)
	return models.Module{}
}

// TestModpackAuthoringFoldsDownWithoutSubsystem: end-user authoring with the
// subsystem off is not a state anyone can act on, and storing it would take
// effect silently the day modpacks are switched on.
func TestModpackAuthoringFoldsDownWithoutSubsystem(t *testing.T) {
	h, fake := newFeatureSettingsFixture(t, nil)
	putFeatures(t, h, `{"modpacks":false,"modpackAuthoring":true}`)

	if got := fake.kv["feature_modpack_authoring_enabled"]; got != "false" {
		t.Errorf("feature_modpack_authoring_enabled = %q, want false when modpacks are off", got)
	}
}

// TestModpackModuleRowDerivedFromFlags: the navbar row must never claim a wider
// audience than the gate allows. admin-only with the subsystem on and authoring
// closed, everyone once authoring opens, and gone from the navbar with the
// subsystem off.
func TestModpackModuleRowDerivedFromFlags(t *testing.T) {
	h, fake := newFeatureSettingsFixture(t, nil)

	putFeatures(t, h, `{"modpacks":true,"modpackAuthoring":false}`)
	m := findModule(t, fake, modpackModuleName)
	if !m.IsEnabled || m.AccessRole != "admin" {
		t.Errorf("subsystem on, authoring off: row = {enabled:%v role:%q}, want {true admin}", m.IsEnabled, m.AccessRole)
	}
	if m.IsSystem {
		t.Error("the Modpacks row must be non-system; seedSystemModules deletes a system row of that name")
	}

	putFeatures(t, h, `{"modpacks":true,"modpackAuthoring":true}`)
	m = findModule(t, fake, modpackModuleName)
	if !m.IsEnabled || m.AccessRole != "all" {
		t.Errorf("authoring on: row = {enabled:%v role:%q}, want {true all}", m.IsEnabled, m.AccessRole)
	}

	putFeatures(t, h, `{"modpacks":false}`)
	m = findModule(t, fake, modpackModuleName)
	if m.IsEnabled || m.AccessRole != "admin" {
		t.Errorf("subsystem off: row = {enabled:%v role:%q}, want {false admin}", m.IsEnabled, m.AccessRole)
	}

	if len(fake.modules) != 1 {
		t.Errorf("got %d module rows, want exactly 1 - a sync must reuse the row, not add one", len(fake.modules))
	}
}

// TestModpackAuthoringBulkApply pins the three things that make the manual marker
// worth having: the bulk write only fires on a real transition, it carries the
// admin's include-manual choice through, and the value it applies is the flag's.
func TestModpackAuthoringBulkApply(t *testing.T) {
	t.Run("no transition, no bulk write", func(t *testing.T) {
		h, fake := newFeatureSettingsFixture(t, map[string]string{
			"feature_modpacks_enabled":          "true",
			"feature_modpack_authoring_enabled": "true",
		})
		putFeatures(t, h, `{"modpacks":true,"modpackAuthoring":true}`)
		if len(fake.bulkCalls) != 0 {
			t.Errorf("bulk calls = %+v, want none: re-saving an unchanged form must not re-flatten per-user rows", fake.bulkCalls)
		}
	})

	t.Run("opening authoring applies true and skips manual rows by default", func(t *testing.T) {
		h, fake := newFeatureSettingsFixture(t, map[string]string{"feature_modpacks_enabled": "true"})
		rec := putFeatures(t, h, `{"modpacks":true,"modpackAuthoring":true}`)
		if len(fake.bulkCalls) != 1 {
			t.Fatalf("bulk calls = %+v, want exactly 1", fake.bulkCalls)
		}
		if !fake.bulkCalls[0].can {
			t.Error("bulk value = false, want true (it must follow the flag)")
		}
		if fake.bulkCalls[0].includeManual {
			t.Error("includeManual defaulted to true; an unspecified request must not overwrite hand-set rows")
		}
		var resp struct {
			UsersChanged int64 `json:"usersChanged"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.UsersChanged == 0 {
			t.Error("usersChanged not reported; the admin has no way to see what the switch did")
		}
	})

	t.Run("closing authoring applies false and honours includeManual", func(t *testing.T) {
		h, fake := newFeatureSettingsFixture(t, map[string]string{
			"feature_modpacks_enabled":          "true",
			"feature_modpack_authoring_enabled": "true",
		})
		putFeatures(t, h, `{"modpacks":true,"modpackAuthoring":false,"applyAuthoringToManual":true}`)
		if len(fake.bulkCalls) != 1 {
			t.Fatalf("bulk calls = %+v, want exactly 1", fake.bulkCalls)
		}
		if fake.bulkCalls[0].can {
			t.Error("bulk value = true, want false")
		}
		if !fake.bulkCalls[0].includeManual {
			t.Error("includeManual was dropped; the admin explicitly asked to include hand-set rows")
		}
	})
}

// TestModpackModuleRole is the derivation on its own: the role follows authoring,
// nothing else.
func TestModpackModuleRole(t *testing.T) {
	if got := modpackModuleRole(true); got != "all" {
		t.Errorf("modpackModuleRole(true) = %q, want all", got)
	}
	if got := modpackModuleRole(false); got != "admin" {
		t.Errorf("modpackModuleRole(false) = %q, want admin", got)
	}
}
