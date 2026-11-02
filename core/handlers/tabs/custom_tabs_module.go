package tabs

import (
	"dylaris-core/models"
	"dylaris-core/store"
)

// customTabsModuleName is the modules-table row that puts Custom Tabs in the
// navbar. Seeded by database/db_tables.go.
const customTabsModuleName = "Custom Tabs"

// customTabsModulePosition is where a freshly created row lands: after Modpacks.
// Only the initial value; the admin reorders by drag-and-drop and a sync never
// overwrites that.
const customTabsModulePosition = 7

// syncCustomTabsModule brings the Custom Tabs navbar row in line with the
// tab-proxy settings, creating it if an install predates the row.
//
// is_enabled and access_role are DERIVED here, which is why Settings -> Modules
// renders them read-only for this row: an edit there would be silently undone
// the next time the tab-proxy settings are saved, and a control that quietly
// loses is worse than no control. Position stays the admin's.
//
// Same shape as syncModpackModule, deliberately - these are the only two rows
// that are answers to a feature question rather than settings of their own, and
// they should not drift into two different mechanisms.
func syncCustomTabsModule(st store.Store, enabled bool, audience string) error {
	if st == nil {
		return nil
	}
	if audience != "admin" && audience != "all" {
		audience = "all"
	}

	modules, err := store.FreshModules(st)
	if err != nil {
		return err
	}
	for i := range modules {
		if modules[i].Name != customTabsModuleName {
			continue
		}
		m := modules[i]
		if m.IsEnabled != enabled {
			if err := st.UpdateModuleStatus(m.ID, enabled); err != nil {
				return err
			}
		}
		if m.AccessRole != audience {
			if err := st.SetModuleAccessRole(m.ID, audience); err != nil {
				return err
			}
		}
		return nil
	}

	_, err = st.CreateModule(&models.Module{
		Name:       customTabsModuleName,
		Type:       "internal",
		Icon:       "layout-dashboard",
		URL:        "/tabs",
		IsEnabled:  enabled,
		IsSystem:   false,
		Position:   customTabsModulePosition,
		AccessRole: audience,
	})
	return err
}
