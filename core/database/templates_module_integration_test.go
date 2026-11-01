package database

import "testing"

// The Templates row is seeded at the old Library position, off and admin-only,
// and follows feature_templates on every boot.
func TestTemplatesModuleFollowsTheFeatureSettingIntegration(t *testing.T) {
	db := freshSchemaDB(t)

	read := func() (enabled bool, role string, position int) {
		t.Helper()
		if err := db.QueryRow(`SELECT is_enabled, access_role, position FROM modules WHERE name = 'Templates' AND url = '/templates'`).Scan(&enabled, &role, &position); err != nil {
			t.Fatalf("read Templates row: %v", err)
		}
		return
	}
	if e, r, p := read(); e || r != "admin" || p != 4 {
		t.Fatalf("fresh seed: enabled=%v role=%q position=%d, want off, admin, 4", e, r, p)
	}

	for _, tc := range []struct {
		setting string
		enabled bool
		role    string
	}{{"admin", true, "admin"}, {"all", true, "all"}, {"off", false, "admin"}} {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('feature_templates', $1)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, tc.setting); err != nil {
			t.Fatalf("set feature_templates: %v", err)
		}
		seedSystemModules(db)
		if err := applyDerivedModuleRows(db); err != nil {
			t.Fatalf("derive module rows: %v", err)
		}
		if e, r, _ := read(); e != tc.enabled || r != tc.role {
			t.Errorf("feature_templates=%s: enabled=%v role=%q, want %v %q", tc.setting, e, r, tc.enabled, tc.role)
		}
	}
}
