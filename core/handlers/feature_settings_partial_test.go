package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// The whole point of the split screen: a tab saves its own half.
//
// The Features page used to be one card with eleven switches behind a single
// save, so it always sent every flag. Cut into tabs, each tab sends only what it
// shows - and if an absent field still counted as false, opening the API-keys
// tab and saving it would switch Tickets, Modpacks and BYON off across the
// platform without anyone touching them.
func TestSavingOneFeatureLeavesTheOthersAlone(t *testing.T) {
	kv := map[string]string{
		"feature_tickets_enabled":  "true",
		"feature_modpacks_enabled": "true",
		"feature_byon_enabled":     "true",
		"apikeys_user_enabled":     "false",
	}
	h, st := newFeatureSettingsFixture(t, kv)

	putFeatures(t, h, `{"userApiKeys":true}`)

	if st.kv["apikeys_user_enabled"] != "true" {
		t.Errorf("the submitted flag was not written: %q", st.kv["apikeys_user_enabled"])
	}
	for _, key := range []string{"feature_tickets_enabled", "feature_modpacks_enabled", "feature_byon_enabled"} {
		if st.kv[key] != "true" {
			t.Errorf("%s = %q after a request that never mentioned it, want true", key, st.kv[key])
		}
	}
}

// An empty allowed-capability list means "no extra restriction", so it cannot
// double as "not sent" - a tab that does not show the capability matrix would
// otherwise clear it on every save.
func TestSavingWithoutTheCapabilityListLeavesItIntact(t *testing.T) {
	kv := map[string]string{"apikeys_user_allowed_caps": "servers.read,servers.write"}
	h, st := newFeatureSettingsFixture(t, kv)

	putFeatures(t, h, `{"tickets":false}`)
	if st.kv["apikeys_user_allowed_caps"] != "servers.read,servers.write" {
		t.Errorf("the capability list was rewritten to %q by a request that never sent it", st.kv["apikeys_user_allowed_caps"])
	}

	// And sending it empty DOES clear it, or the restriction could never be
	// removed once set.
	putFeatures(t, h, `{"userApiKeyAllowedCaps":""}`)
	if st.kv["apikeys_user_allowed_caps"] != "" {
		t.Errorf("an explicit empty list did not clear the restriction: %q", st.kv["apikeys_user_allowed_caps"])
	}
}

// Switching Modpacks off has to take end-user authoring with it even though the
// request never mentions authoring - otherwise the flag sits there as true and
// takes effect the day somebody switches modpacks back on.
func TestSwitchingModpacksOffAlsoClosesAuthoringItDidNotMention(t *testing.T) {
	kv := map[string]string{
		"feature_modpacks_enabled":          "true",
		"feature_modpack_authoring_enabled": "true",
	}
	h, st := newFeatureSettingsFixture(t, kv)

	putFeatures(t, h, `{"modpacks":false}`)
	if st.kv["feature_modpack_authoring_enabled"] != "false" {
		t.Errorf("authoring = %q after modpacks was switched off, want false", st.kv["feature_modpack_authoring_enabled"])
	}
}

// Tickets and Library: one switch each, not two.
//
// With the ticket feature ON and the module row OFF, every ticket endpoint
// worked and nothing in the panel led to it - people arrived only through a
// notification link. The row now follows the flag in both directions.
//
// Enabling tickets needs Core file storage, so the ON direction is driven
// through Library, which shares the same code path and has no precondition.
func TestAModuleRowFollowsItsFeatureFlagInBothDirections(t *testing.T) {
	h, st := newFeatureSettingsFixture(t, map[string]string{"feature_tickets_enabled": "true"})
	st.modules = []models.Module{
		{ID: 5, Name: "Tickets", IsEnabled: true, AccessRole: "all"},
		{ID: 4, Name: "Library", IsEnabled: false, AccessRole: "admin"},
	}

	putFeatures(t, h, `{"library":true}`)
	if !findModule(t, st, "Library").IsEnabled {
		t.Error("the Library row stayed off while its feature was switched on")
	}
	// The audience is NOT derived: it stays whatever the operator chose in
	// Settings -> Modules, where Library's All/Admin choice has real meaning.
	if role := findModule(t, st, "Library").AccessRole; role != "admin" {
		t.Errorf("the audience was rewritten to %q; only is_enabled is derived", role)
	}
	// A request about the library must not touch any other row.
	if !findModule(t, st, "Tickets").IsEnabled {
		t.Error("enabling the library switched the Tickets row off")
	}

	putFeatures(t, h, `{"tickets":false}`)
	if findModule(t, st, "Tickets").IsEnabled {
		t.Error("the Tickets row stayed on after its feature was switched off")
	}
}

// GET has to state every flag: the panel renders switches from it, and a
// missing field would render as off and then be saved as off.
func TestFeaturesGetStatesEveryFlag(t *testing.T) {
	h, _ := newFeatureSettingsFixture(t, map[string]string{})
	w := httptest.NewRecorder()
	h.Get(w, httptest.NewRequest(http.MethodGet, "/api/admin/settings/features", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d", w.Code)
	}
	var resp struct {
		Features map[string]json.RawMessage `json:"features"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tickets", "modpacks", "library", "modpackAuthoring", "autoMove", "byon", "userApiKeys", "userApiKeyAllowedCaps"} {
		raw, ok := resp.Features[field]
		if !ok || string(raw) == "null" {
			t.Errorf("GET omitted %q; the panel would render it as off and save it as off", field)
		}
	}
}
