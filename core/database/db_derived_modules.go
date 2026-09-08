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
// navigation led to it. Library had no flag at all.
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
	// The Library flag is seeded from the row rather than from a constant, so an
	// install that had the library switched on keeps it on across this upgrade.
	// A hardcoded default would have silently closed it - or silently opened it -
	// on the boot that introduced the flag.
	if _, err := db.Exec(`INSERT INTO settings (key, value)
		SELECT 'feature_library_enabled',
		       CASE WHEN COALESCE((SELECT is_enabled FROM modules WHERE name = 'Library'), FALSE)
		            THEN 'true' ELSE 'false' END
		ON CONFLICT (key) DO NOTHING`); err != nil {
		return fmt.Errorf("derived modules: seed feature_library_enabled: %w", err)
	}

	// Only is_enabled for these two. Who sees them stays the operator's answer
	// in Settings -> Modules, and Library's audience in particular carries real
	// meaning there (admin-only, or a browsable tab for users).
	for _, m := range []struct{ module, key string }{
		{"Tickets", "feature_tickets_enabled"},
		{"Library", "feature_library_enabled"},
	} {
		if _, err := db.Exec(`UPDATE modules
			SET is_enabled = COALESCE((SELECT value = 'true' FROM settings WHERE key = $2), FALSE)
			WHERE name = $1`, m.module, m.key); err != nil {
			return fmt.Errorf("derived modules: re-derive %s: %w", m.module, err)
		}
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
	return nil
}
