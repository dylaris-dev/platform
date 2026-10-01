package database

import (
	"database/sql"
	"testing"

	"dylaris-core/models"
)

// Against a real Postgres, because both guards are SQL: the panel role moves
// in the same UPDATE as the role, keyed on the row as it was, and the insert
// writes role and panel role itself. A fake could not tell whether Postgres
// even accepts the statement.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationPanelRoleFollowsTheRole(t *testing.T) {
	db, st := integrationDB(t)

	panelRole := func(t *testing.T, id string) (string, bool) {
		t.Helper()
		var name sql.NullString
		if err := db.QueryRow(`SELECT pr.name FROM users u LEFT JOIN panel_roles pr ON pr.id = u.panel_role_id WHERE u.id = $1`, id).Scan(&name); err != nil {
			t.Fatalf("read panel role: %v", err)
		}
		return name.String, name.Valid
	}
	roleOf := func(t *testing.T, id string) string {
		t.Helper()
		var role string
		if err := db.QueryRow(`SELECT role FROM users WHERE id = $1`, id).Scan(&role); err != nil {
			t.Fatalf("read role: %v", err)
		}
		return role
	}

	// An admin created here used to be role 'user' until the next boot, and the
	// edit dialog's next save demoted them.
	admin := &models.User{Username: uniqueName("adm_"), Password: "x", Email: uniqueName("adm_") + "@example.test", IsAdmin: true, Is2FAEnabled: true}
	if err := st.CreateUser(admin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, admin.ID) })
	if r := roleOf(t, admin.ID); r != "admin" {
		t.Fatalf("a created admin has role %q, want admin", r)
	}
	if name, ok := panelRole(t, admin.ID); !ok || name != "admin" {
		t.Fatalf("a created admin has panel role %q (%v), want the admin role", name, ok)
	}
	var tfa bool
	db.QueryRow(`SELECT is_2fa_enabled FROM users WHERE id = $1`, admin.ID).Scan(&tfa)
	if tfa {
		t.Error("an account was created with 2FA on and no secret: it could never sign in")
	}

	// A save that keeps the role keeps the panel role.
	if err := st.SetUserRole(admin.ID, "admin"); err != nil {
		t.Fatalf("SetUserRole same: %v", err)
	}
	if name, ok := panelRole(t, admin.ID); !ok || name != "admin" {
		t.Fatalf("an unchanged role lost its panel role: %q", name)
	}

	overrides := func(t *testing.T) string {
		t.Helper()
		var o string
		if err := db.QueryRow(`SELECT panel_cap_overrides::text FROM users WHERE id = $1`, admin.ID).Scan(&o); err != nil {
			t.Fatalf("read overrides: %v", err)
		}
		return o
	}
	if _, err := db.Exec(`UPDATE users SET panel_cap_overrides = '{"grant":["users.write"]}'::jsonb WHERE id = $1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserRole(admin.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if o := overrides(t); o == "{}" {
		t.Fatal("an unchanged role dropped the per-user overrides")
	}

	// The demotion: the admin panel role carried every staff capability and
	// stayed behind, and so did any capability granted per user, which the
	// resolver honours without a panel role.
	if err := st.SetUserRole(admin.ID, "user"); err != nil {
		t.Fatalf("SetUserRole demote: %v", err)
	}
	if name, ok := panelRole(t, admin.ID); ok {
		t.Fatalf("a demoted admin kept the %q panel role", name)
	}
	if o := overrides(t); o != "{}" {
		t.Fatalf("a demoted admin kept per-user overrides %s", o)
	}

	if err := st.SetUserRole(admin.ID, "support"); err != nil {
		t.Fatalf("SetUserRole support: %v", err)
	}
	if name, ok := panelRole(t, admin.ID); !ok || name != "support" {
		t.Fatalf("support got panel role %q (%v), want support", name, ok)
	}

	// A custom panel role on an ordinary user survives a save that keeps the role.
	var customID int
	if err := db.QueryRow(`INSERT INTO panel_roles (name, capabilities, is_system) VALUES ($1, '[]'::jsonb, FALSE) RETURNING id`, uniqueName("custom_")).Scan(&customID); err != nil {
		t.Fatalf("insert custom role: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM panel_roles WHERE id = $1`, customID) })
	if err := st.SetUserRole(admin.ID, "user"); err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE users SET panel_role_id = $2 WHERE id = $1`, admin.ID, customID)
	if err := st.SetUserRole(admin.ID, "user"); err != nil {
		t.Fatal(err)
	}
	var kept sql.NullInt64
	db.QueryRow(`SELECT panel_role_id FROM users WHERE id = $1`, admin.ID).Scan(&kept)
	if !kept.Valid || int(kept.Int64) != customID {
		t.Fatalf("an unchanged role dropped the custom panel role: %v", kept)
	}
}
