package tabs

import (
	"dylaris-core/handlers"
	"dylaris-core/models"
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
func syncCustomTabsModule(state *handlers.AppState, enabled bool, audience string) error {
	if state == nil || state.Store == nil {
		return nil
	}
	if audience != "admin" && audience != "all" {
		audience = "all"
	}

	modules, err := state.Store.ListModules()
	if err != nil {
		return err
	}
	for i := range modules {
		if modules[i].Name != customTabsModuleName {
			continue
		}
		m := modules[i]
		if m.IsEnabled != enabled {
			if err := state.Store.UpdateModuleStatus(m.ID, enabled); err != nil {
				return err
			}
		}
		if m.AccessRole != audience {
			if err := state.Store.SetModuleAccessRole(m.ID, audience); err != nil {
				return err
			}
		}
		return nil
	}

	_, err = state.Store.CreateModule(&models.Module{
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
