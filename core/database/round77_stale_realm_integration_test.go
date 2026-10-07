package database

import (
	"strings"
	"sync"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// An admin owner change moves servers.owner_id and leaves the grant rows and
// the roles they point at behind. The resolver ignores them since round 76;
// the roster, the inherited list and the old owner's Access page listed them
// as live access. Against a real Postgres because the filter IS the SQL.
func TestIntegrationRosterHidesAPreviousOwnersRows(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	mk := func(p string) *models.User {
		u := &models.User{Username: uniqueName(p), Password: "x", Email: uniqueName(p+"e") + "@example.test"}
		if err := st.CreateUser(u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { st.DeleteUser(u.ID) })
		return u
	}
	friend, newOwner := mk("u_r77f_"), mk("u_r77o_")

	roleID, err := st.CreateServerRole(f.user.ID, uniqueName("r77role_"), []string{"files.write"})
	if err != nil {
		t.Fatalf("CreateServerRole: %v", err)
	}
	if err := st.UpsertServerGrant(&f.server.ID, friend.ID, f.user.ID, &roleID,
		store.CapOverrides{Grant: []string{"console.read"}}, false); err != nil {
		t.Fatalf("UpsertServerGrant: %v", err)
	}

	// Before the move the friend is listed, role caps included: the filter
	// must not hide a live row.
	members, err := st.ListInvitesByServer(f.server.ID)
	if err != nil || len(members) != 1 || !hasCap(members[0].Capabilities, "files.write") {
		t.Fatalf("before the move: members = %+v, err = %v", members, err)
	}
	if _, err := st.GetInvite(f.server.ID, friend.ID); err != nil {
		t.Fatalf("before the move: GetInvite: %v", err)
	}
	if g, _ := st.ListGrantsByOwner(f.user.ID); len(g) != 1 {
		t.Fatalf("before the move: old owner's grants = %d, want 1", len(g))
	}

	if err := st.UpdateServerOwner(f.server.ID, &newOwner.ID); err != nil {
		t.Fatalf("UpdateServerOwner: %v", err)
	}

	if members, _ := st.ListInvitesByServer(f.server.ID); len(members) != 0 {
		t.Errorf("roster after the move lists %+v, want nobody", members)
	}
	if _, err := st.GetInvite(f.server.ID, friend.ID); err == nil {
		t.Errorf("GetInvite after the move found the previous owner's row")
	}
	if g, _ := st.ListGrantsByOwner(f.user.ID); len(g) != 0 {
		t.Errorf("old owner's Access page still lists %d grants on a server they gave away", len(g))
	}
	if c, _ := st.CountInvitesPerServer(); c[f.server.ID] != 0 {
		t.Errorf("admin server list counts %d members after the move, want 0", c[f.server.ID])
	}

	// The new owner re-grants the same friend on the same row (the upsert sets
	// owner_user_id, keeps the role id): the row is live again, the previous
	// owner's role is not.
	if err := st.UpsertServerGrant(&f.server.ID, friend.ID, newOwner.ID, &roleID,
		store.CapOverrides{Grant: []string{"console.read"}}, false); err != nil {
		t.Fatalf("UpsertServerGrant by new owner: %v", err)
	}
	members, _ = st.ListInvitesByServer(f.server.ID)
	if len(members) != 1 {
		t.Fatalf("after re-grant: members = %d, want 1", len(members))
	}
	if hasCap(members[0].Capabilities, "files.write") || !hasCap(members[0].Capabilities, "console.read") {
		t.Errorf("after re-grant capabilities = %v, want console.read without the old owner's role", members[0].Capabilities)
	}
	if g, _ := st.ListGrantsByOwner(newOwner.ID); len(g) != 1 || g[0].ServerRoleName != "" {
		t.Errorf("new owner's Access page = %+v, want one grant without the old owner's role name", g)
	}
	if c, _ := st.CountInvitesPerServer(); c[f.server.ID] != 1 {
		t.Errorf("admin server list counts %d members after re-grant, want 1", c[f.server.ID])
	}
}

func hasCap(caps []string, c string) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// Two replicas editing one settings list each wrote back what they had read,
// so one change was lost - for the demo list, a deleted server put back on it.
func TestIntegrationUpdateSettingLosesNoConcurrentEdit(t *testing.T) {
	db, st := integrationDB(t)
	key := uniqueName("r77_list_")
	t.Cleanup(func() { db.Exec(`DELETE FROM settings WHERE key = $1`, key) })

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := st.UpdateSetting(key, func(old string) (string, error) {
				return old + string(rune('a'+i)), nil
			})
			if err != nil {
				t.Errorf("UpdateSetting: %v", err)
			}
		}(i)
	}
	wg.Wait()
	got, err := st.GetSetting(key)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	for i := 0; i < n; i++ {
		if !strings.ContainsRune(got, rune('a'+i)) {
			t.Fatalf("value %q lost edit %c", got, rune('a'+i))
		}
	}
}
