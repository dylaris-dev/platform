package database

import (
	"database/sql"
	"fmt"
)

// applyDerivedModuleRows makes every module row that a feature flag owns follow
// that flag, on every boot rather than only when someone saves the form.
//
// The platform had three different wirings for one idea. Modpacks derived its
// row from its flags AND re-derived at boot. Custom Tabs derived its row but
// only at write time. Tickets derived nothing: two switches in two screens, and
// with the feature on and the row off the entire ticket API worked while no
// navigation led to it.
//
// Write-time-only derivation is the interesting failure. It looks correct
// because it is correct at the moment it runs, and then nothing re-reads the
// flag unless an admin happens to open that form again - so a row that got out
// of step stays out of step forever. Custom Tabs still has that window: an
// install whose tab-proxy flag was set before the row existed gets the hardcoded
// off row and no navbar entry until Features is saved again. This closes it for
// all three.
//
// Runs AFTER seedSystemModules, which is what guarantees the rows exist.
func applyDerivedModuleRows(db *sql.DB) error {
	// Only is_enabled for Tickets. Who sees it stays the operator's answer in
	// Settings -> Modules.
	if _, err := db.Exec(`UPDATE modules
		SET is_enabled = COALESCE((SELECT value = 'true' FROM settings WHERE key = 'feature_tickets_enabled'), FALSE)
		WHERE name = 'Tickets'`); err != nil {
		return fmt.Errorf("derived modules: re-derive Tickets: %w", err)
	}

	// Custom Tabs derives its audience too, from a setting with no other home.
	// An unset audience means everyone, matching the panel's own default.
	if _, err := db.Exec(`UPDATE modules SET
			is_enabled = COALESCE((SELECT value = 'true' FROM settings WHERE key = 'feature_tab_proxy_enabled'), FALSE),
			access_role = CASE WHEN COALESCE((SELECT value FROM settings WHERE key = 'tab_proxy_audience'), 'all') = 'admin'
			                   THEN 'admin' ELSE 'all' END
		WHERE name = 'Custom Tabs'`); err != nil {
		return fmt.Errorf("derived modules: re-derive Custom Tabs: %w", err)
	}

	// Templates derives enabled and audience from feature_templates; an unset
	// setting is off. 'off' stays admin-only so a later enable starts narrow.
	if _, err := db.Exec(`UPDATE modules SET
			is_enabled = COALESCE((SELECT value IN ('admin', 'all') FROM settings WHERE key = 'feature_templates'), FALSE),
			access_role = CASE WHEN COALESCE((SELECT value FROM settings WHERE key = 'feature_templates'), 'off') = 'all'
			                   THEN 'all' ELSE 'admin' END
		WHERE name = 'Templates'`); err != nil {
		return fmt.Errorf("derived modules: re-derive Templates: %w", err)
	}
	return nil
}
