package database

import (
	"testing"

	"dylaris-core/models"
)

// Against a real Postgres, because what changed IS the query: who gets told a
// ticket arrived.
//
// It used to ask only the legacy users.role column. Measured on production, an
// account holding the seeded "support" PANEL role - the mechanism the panel's
// role screen offers - was never notified of a new ticket, which is the same
// silence as not being staff at all.
//
// The awkward half is the per-user override: a role that grants the capability
// and an override that takes it back has to come out as NOT staff, while an
// admin with such an override stays staff, because being an admin is its own
// reason. Both are pinned here.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationTicketStaffCoversBothPermissionSystems(t *testing.T) {
	db, st := integrationDB(t)

	mk := func(prefix string) *models.User {
		t.Helper()
		u := &models.User{Username: uniqueName(prefix), Password: "x", Email: uniqueName(prefix) + "@example.test"}
		if err := st.CreateUser(u); err != nil {
			t.Fatalf("CreateUser %s: %v", prefix, err)
		}
		t.Cleanup(func() { st.DeleteUser(u.ID) })
		return u
	}
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// A panel role carrying the ticket capabilities, and one carrying none.
	var supportRoleID, emptyRoleID int
	if err := db.QueryRow(
		`INSERT INTO panel_roles (name, capabilities) VALUES ($1, '["tickets.read","tickets.write"]'::jsonb) RETURNING id`,
		uniqueName("role_sup_")).Scan(&supportRoleID); err != nil {
		t.Fatalf("create support role: %v", err)
	}
	if err := db.QueryRow(
		`INSERT INTO panel_roles (name, capabilities) VALUES ($1, '["users.read"]'::jsonb) RETURNING id`,
		uniqueName("role_none_")).Scan(&emptyRoleID); err != nil {
		t.Fatalf("create empty role: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM panel_roles WHERE id = ANY($1)`, []int{supportRoleID, emptyRoleID})
	})

	legacy := mk("u_legacy_")
	exec(`UPDATE users SET role = 'support' WHERE id = $1`, legacy.ID)

	byRole := mk("u_byrole_")
	exec(`UPDATE users SET panel_role_id = $1 WHERE id = $2`, supportRoleID, byRole.ID)

	byOverride := mk("u_byov_")
	exec(`UPDATE users SET panel_role_id = $1, panel_cap_overrides = '{"grant":["tickets.read"]}'::jsonb WHERE id = $2`,
		emptyRoleID, byOverride.ID)

	denied := mk("u_denied_")
	exec(`UPDATE users SET panel_role_id = $1, panel_cap_overrides = '{"deny":["tickets.read"]}'::jsonb WHERE id = $2`,
		supportRoleID, denied.ID)

	adminDenied := mk("u_admindenied_")
	exec(`UPDATE users SET is_admin = TRUE, panel_cap_overrides = '{"deny":["tickets.read"]}'::jsonb WHERE id = $1`,
		adminDenied.ID)

	plain := mk("u_plain_")

	ids, err := st.ListTicketStaffIDs()
	if err != nil {
		t.Fatalf("ListTicketStaffIDs: %v", err)
	}
	staff := map[string]bool{}
	for _, id := range ids {
		staff[id] = true
	}

	for _, c := range []struct {
		name string
		id   string
		want bool
		why  string
	}{
		{"legacy support role", legacy.ID, true, "the old mechanism must keep working"},
		{"support panel role", byRole.ID, true, "this is the one that was silently excluded"},
		{"capability from a per-user grant", byOverride.ID, true, "an override is a real way to hold a capability"},
		{"capability denied by an override", denied.ID, false, "the role grants it, the override takes it back"},
		{"admin with a deny override", adminDenied.ID, true, "being an admin is its own reason to be told"},
		{"an ordinary user", plain.ID, false, "nothing makes them staff"},
	} {
		if staff[c.id] != c.want {
			t.Errorf("%s: staff = %v, want %v (%s)", c.name, staff[c.id], c.want, c.why)
		}
	}
}
