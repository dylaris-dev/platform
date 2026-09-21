package database

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Against a real Postgres, because what is being checked IS the SQL: the
// roster now reads cap_overrides and the joined role's capabilities, three
// columns and a join it never touched before.
//
// The roster answers "who may do what on my server" and it was answering from
// the legacy permissions blob alone. That blob is empty for everyone added
// through POST /api/grants - the route the panel's Access page uses - so a
// member holding the whole operator set was listed with every flag false.
// Measured on production.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationMemberRosterReportsGrantedCapabilities(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	friend := &models.User{Username: uniqueName("u_fr_"), Password: "x", Email: uniqueName("e_fr_") + "@example.test"}
	if err := st.CreateUser(friend); err != nil {
		t.Fatalf("CreateUser friend: %v", err)
	}
	t.Cleanup(func() { st.DeleteUser(friend.ID) })

	// Exactly what the Access page writes: capability overrides, no legacy
	// permission blob at all.
	operator := []string{
		"overview.read", "stats.read", "console.read", "console.send",
		"power.start", "power.stop", "power.restart",
		"rcon.exec", "players.read", "players.manage",
	}
	if err := st.UpsertServerGrant(&f.server.ID, friend.ID, f.user.ID, nil,
		store.CapOverrides{Grant: operator}, false); err != nil {
		t.Fatalf("UpsertServerGrant: %v", err)
	}

	members, err := st.ListInvitesByServer(f.server.ID)
	if err != nil {
		t.Fatalf("ListInvitesByServer: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	m := members[0]

	if len(m.Capabilities) != len(operator) {
		t.Errorf("capabilities = %v, want the %d granted", m.Capabilities, len(operator))
	}
	// The booleans are the summary the roster has always printed. Every one of
	// these was false before the change, for a member who could restart the
	// server and type on its console.
	if !m.Permissions.Console || !m.Permissions.Power || !m.Permissions.Players || !m.Permissions.Overview {
		t.Errorf("permissions = %+v, want console, power, players and overview", m.Permissions)
	}
	// And the ones they were NOT given must stay off, or the roster would lie
	// the other way.
	if m.Permissions.Files || m.Permissions.Members || m.Permissions.Network || m.Permissions.Backups {
		t.Errorf("permissions = %+v grants tabs the member was never given", m.Permissions)
	}
}

// A role carries its capabilities in another table, so the join is the only
// way the roster sees them. Advanced permissions mode is the only thing that
// creates one today, which is exactly why it needs a test rather than a
// production sighting.
func TestIntegrationMemberRosterIncludesRoleCapabilities(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	friend := &models.User{Username: uniqueName("u_fr2_"), Password: "x", Email: uniqueName("e_fr2_") + "@example.test"}
	if err := st.CreateUser(friend); err != nil {
		t.Fatalf("CreateUser friend: %v", err)
	}
	t.Cleanup(func() { st.DeleteUser(friend.ID) })

	roleID, err := st.CreateServerRole(f.user.ID, uniqueName("role_"), []string{"files.read", "backups.read"})
	if err != nil {
		t.Fatalf("CreateServerRole: %v", err)
	}
	t.Cleanup(func() { st.DeleteServerRole(roleID, f.user.ID) })

	if err := st.UpsertServerGrant(&f.server.ID, friend.ID, f.user.ID, &roleID,
		store.CapOverrides{Grant: []string{"console.read"}, Deny: []string{"backups.read"}}, false); err != nil {
		t.Fatalf("UpsertServerGrant: %v", err)
	}

	members, err := st.ListInvitesByServer(f.server.ID)
	if err != nil {
		t.Fatalf("ListInvitesByServer: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	m := members[0]

	if !m.Permissions.Files {
		t.Errorf("the role's files.read did not reach the roster: %+v (%v)", m.Permissions, m.Capabilities)
	}
	if !m.Permissions.Console {
		t.Errorf("the override's console.read did not reach the roster: %+v", m.Permissions)
	}
	// Deny wins, the same way the resolver applies it.
	if m.Permissions.Backups {
		t.Errorf("a denied capability still shows as granted: %v", m.Capabilities)
	}
}
