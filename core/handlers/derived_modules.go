package handlers

// The modules-table rows these flags own. Names, not ids: the row is seeded by
// name on every boot and an operator can reorder but not rename it.
const (
	ticketsModuleName = "Tickets"
	libraryModuleName = "Library"
)

// A module row's is_enabled follows the feature flag that owns it.
//
// There used to be three different wirings for the same idea. Modpacks and
// Custom Tabs derived their row from their flags; Tickets had two independent
// switches; Servers, Admin, Infrastructure and Library had no flag at all. The
// middle one was the defect: with the ticket feature ON and the module row OFF,
// every ticket endpoint worked and no navigation anywhere led to it, so people
// reached tickets only through a notification link. The reverse - row on, flag
// off - hid the entry and 503'd the API, which at least agreed with itself.
//
// This helper covers the case where only ENABLED is derived and the audience
// stays the operator's, which is the shape Tickets and Library need: whether the
// feature exists is a platform decision, who sees it is a presentation one.
// Modpacks and Custom Tabs keep their own sync because they derive the audience
// too, from a second flag that has no other home.
func syncModuleEnabled(state *AppState, name string, on bool) error {
	if state == nil || state.Store == nil {
		return nil
	}
	modules, err := state.Store.ListModules()
	if err != nil {
		return err
	}
	for i := range modules {
		if modules[i].Name != name {
			continue
		}
		if modules[i].IsEnabled == on {
			return nil
		}
		return state.Store.UpdateModuleStatus(modules[i].ID, on)
	}
	// No row: seedSystemModules creates these on every boot, so a missing one
	// means the platform has not finished starting. Creating it here would race
	// that seed and could duplicate the row, and the boot re-derive will put it
	// right anyway.
	return nil
}
