package tabs

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// customTabsFakeStore embeds store.Store (nil) so it satisfies the interface at
// compile time; only the four module calls the sync makes are overridden.
type customTabsFakeStore struct {
	store.Store
	mods    []models.Module
	created *models.Module
	status  map[int]bool
	roles   map[int]string
}

func (f *customTabsFakeStore) ListModules() ([]models.Module, error) { return f.mods, nil }

func (f *customTabsFakeStore) UpdateModuleStatus(id int, enabled bool) error {
	if f.status == nil {
		f.status = map[int]bool{}
	}
	f.status[id] = enabled
	return nil
}

func (f *customTabsFakeStore) SetModuleAccessRole(id int, role string) error {
	if f.roles == nil {
		f.roles = map[int]string{}
	}
	f.roles[id] = role
	return nil
}

func (f *customTabsFakeStore) CreateModule(m *models.Module) (int, error) {
	f.created = m
	return 42, nil
}

// The navbar row follows the tab-proxy settings, in both fields.
//
// This is the whole reason Settings -> Modules renders them read-only for this
// row: an edit there is undone the next time this card is saved, and a control
// that quietly loses is worse than no control. If the sync ever stops writing
// one of the two, that read-only notice becomes a lie.
func TestCustomTabsModuleFollowsTheTabProxySettings(t *testing.T) {
	cases := []struct {
		name        string
		enabled     bool
		audience    string
		wantEnabled bool
		wantRole    string
	}{
		{"off and admin-only", false, "admin", false, "admin"},
		{"on and everyone", true, "all", true, "all"},
		{"on but admin-only", true, "admin", true, "admin"},
		// An unknown audience is corrected, not stored. It reaches this from a
		// two-button control, so anything else is a caller bug - and defaulting
		// to "all" keeps the row matching TabProxyAudience, which does the same.
		{"garbage audience falls back to all", true, "everyone", true, "all"},
		{"empty audience falls back to all", true, "", true, "all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &customTabsFakeStore{mods: []models.Module{
				{ID: 9, Name: "Custom Tabs", IsEnabled: !tc.wantEnabled, AccessRole: "nonsense"},
			}}
			if err := syncCustomTabsModule(f, tc.enabled, tc.audience); err != nil {
				t.Fatalf("sync: %v", err)
			}
			if got, ok := f.status[9]; !ok || got != tc.wantEnabled {
				t.Errorf("is_enabled = %v (written: %v), want %v", got, ok, tc.wantEnabled)
			}
			if got := f.roles[9]; got != tc.wantRole {
				t.Errorf("access_role = %q, want %q", got, tc.wantRole)
			}
			if f.created != nil {
				t.Error("created a second row while one already existed")
			}
		})
	}
}

// An install that predates the row gets one, rather than silently having no
// navbar entry no matter what the settings say.
func TestCustomTabsModuleIsCreatedWhenMissing(t *testing.T) {
	f := &customTabsFakeStore{mods: []models.Module{{ID: 1, Name: "Servers"}}}
	if err := syncCustomTabsModule(f, true, "admin"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if f.created == nil {
		t.Fatal("no row was created")
	}
	if f.created.Name != "Custom Tabs" || !f.created.IsEnabled || f.created.AccessRole != "admin" {
		t.Errorf("created %+v, want an enabled admin-only Custom Tabs row", f.created)
	}
	if f.created.IsSystem {
		t.Error("created is_system=true, which would also block DISABLING it")
	}
}
