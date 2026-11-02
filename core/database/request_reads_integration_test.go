package database

import (
	"testing"

	"dylaris-core/store"
)

// The single-server reads carry the node's owner from the same query, so the
// resolver's foreign-node fence needs no node read of its own.
func TestIntegrationServerReadCarriesNodeOwner(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	srv, err := st.GetServerByID(f.server.ID)
	if err != nil {
		t.Fatalf("GetServerByID: %v", err)
	}
	if !srv.NodeOwnerLoaded || srv.NodeOwnerID != nil {
		t.Fatalf("platform node: loaded=%v owner=%v, want loaded and no owner", srv.NodeOwnerLoaded, srv.NodeOwnerID)
	}

	if _, err := db.Exec(`UPDATE nodes SET owner_id = $1 WHERE id = $2`, f.user.ID, f.node.ID); err != nil {
		t.Fatalf("set node owner: %v", err)
	}
	t.Cleanup(func() { db.Exec(`UPDATE nodes SET owner_id = NULL WHERE id = $1`, f.node.ID) })
	byID, err := st.GetServerByID(f.server.ID)
	if err != nil {
		t.Fatalf("GetServerByID: %v", err)
	}
	byUUID, err := st.GetServerByUUID(f.server.UUID)
	if err != nil {
		t.Fatalf("GetServerByUUID: %v", err)
	}
	for name, got := range map[string]*string{"by id": byID.NodeOwnerID, "by uuid": byUUID.NodeOwnerID} {
		if got == nil || *got != f.user.ID {
			t.Errorf("%s: node owner %v, want %s", name, got, f.user.ID)
		}
	}
}

// The one-statement grant read returns what the two separate reads return,
// with the role joined, and nothing that belongs to another user or realm.
func TestIntegrationServerAndAccountGrantsInOneRead(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	other := newFixture(t, st) // a second owner, server and user

	member := other.user // invited into f's realm
	roleID, err := st.CreateServerRole(f.user.ID, uniqueName("role_"), []string{"console.read"})
	if err != nil {
		t.Fatalf("CreateServerRole: %v", err)
	}
	sid := f.server.ID
	if err := st.UpsertServerGrant(&sid, member.ID, f.user.ID, &roleID, store.CapOverrides{Grant: []string{"stats.read"}}, true); err != nil {
		t.Fatalf("direct grant: %v", err)
	}
	if err := st.UpsertServerGrant(nil, member.ID, f.user.ID, nil, store.CapOverrides{Grant: []string{"modpacks.read"}}, false); err != nil {
		t.Fatalf("account grant: %v", err)
	}
	// Noise: the member's grant in ANOTHER realm, and another user's grant here.
	osid := other.server.ID
	if err := st.UpsertServerGrant(nil, member.ID, other.user.ID, nil, store.CapOverrides{Grant: []string{"x"}}, false); err != nil {
		t.Fatalf("foreign account grant: %v", err)
	}
	if err := st.UpsertServerGrant(&sid, f.user.ID, f.user.ID, nil, store.CapOverrides{}, false); err != nil {
		t.Fatalf("other user's grant: %v", err)
	}
	t.Cleanup(func() {
		st.DeleteServerGrant(&sid, f.user.ID, member.ID)
		st.DeleteServerGrant(nil, f.user.ID, member.ID)
		st.DeleteServerGrant(nil, other.user.ID, member.ID)
		st.DeleteServerGrant(&sid, f.user.ID, f.user.ID)
	})

	direct, account, err := st.GetServerAndAccountGrants(sid, f.user.ID, member.ID)
	if err != nil {
		t.Fatalf("GetServerAndAccountGrants: %v", err)
	}
	wantDirect, err := st.GetServerGrant(sid, member.ID)
	if err != nil {
		t.Fatalf("GetServerGrant: %v", err)
	}
	wantAccount, err := st.GetAccountGrant(f.user.ID, member.ID)
	if err != nil {
		t.Fatalf("GetAccountGrant: %v", err)
	}
	if direct == nil || *direct.ServerID != *wantDirect.ServerID || direct.OwnerUserID != wantDirect.OwnerUserID ||
		*direct.ServerRoleID != *wantDirect.ServerRoleID || !direct.Inherit ||
		len(direct.CapOverrides.Grant) != 1 || direct.CapOverrides.Grant[0] != "stats.read" {
		t.Fatalf("direct %+v, want %+v", direct, wantDirect)
	}
	if direct.Role == nil || direct.Role.ID != roleID || direct.Role.OwnerUserID != f.user.ID ||
		len(direct.Role.Capabilities) != 1 || direct.Role.Capabilities[0] != "console.read" {
		t.Fatalf("joined role %+v", direct.Role)
	}
	if account == nil || account.ServerID != nil || account.OwnerUserID != wantAccount.OwnerUserID ||
		len(account.CapOverrides.Grant) != 1 || account.CapOverrides.Grant[0] != "modpacks.read" || account.Role != nil {
		t.Fatalf("account %+v, want %+v", account, wantAccount)
	}

	// Nobody invited: both nil, no error.
	d, a, err := st.GetServerAndAccountGrants(osid, other.user.ID, f.user.ID)
	if err != nil || d != nil || a != nil {
		t.Fatalf("stranger: %+v %+v %v", d, a, err)
	}
}

// The one-statement N+1 replacements answer what the per-row reads answered.
func TestIntegrationBatchReadsMatchPerRowReads(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	other := newFixture(t, st)

	counts, err := st.CountServersPerNode()
	if err != nil {
		t.Fatalf("CountServersPerNode: %v", err)
	}
	for _, n := range []int{f.node.ID, other.node.ID} {
		want, err := st.CountServersByNode(n)
		if err != nil {
			t.Fatalf("CountServersByNode: %v", err)
		}
		if counts[n] != want {
			t.Errorf("node %d: %d, per-node count %d", n, counts[n], want)
		}
	}

	member := other.user
	roleID, err := st.CreateServerRole(f.user.ID, uniqueName("role_"), []string{"console.read"})
	if err != nil {
		t.Fatalf("CreateServerRole: %v", err)
	}
	sid := f.server.ID
	if err := st.UpsertServerGrant(&sid, member.ID, f.user.ID, &roleID, store.CapOverrides{}, false); err != nil {
		t.Fatalf("direct grant: %v", err)
	}
	if err := st.UpsertServerGrant(nil, member.ID, f.user.ID, nil, store.CapOverrides{Grant: []string{"stats.read"}}, false); err != nil {
		t.Fatalf("account grant: %v", err)
	}
	t.Cleanup(func() {
		st.DeleteServerGrant(&sid, f.user.ID, member.ID)
		st.DeleteServerGrant(nil, f.user.ID, member.ID)
	})
	grants, err := st.ListGrantsForUser(member.ID)
	if err != nil {
		t.Fatalf("ListGrantsForUser: %v", err)
	}
	var sawDirect, sawAccount bool
	for _, g := range grants {
		if g.UserID != member.ID {
			t.Fatalf("another user's grant listed: %+v", g)
		}
		if g.ServerID != nil && *g.ServerID == sid {
			sawDirect = g.Role != nil && g.Role.ID == roleID && len(g.Role.Capabilities) == 1
		}
		if g.ServerID == nil && g.OwnerUserID == f.user.ID {
			sawAccount = len(g.CapOverrides.Grant) == 1 && g.Role == nil
		}
	}
	if !sawDirect || !sawAccount {
		t.Fatalf("grants %+v: direct-with-role %v, account %v", grants, sawDirect, sawAccount)
	}

	// List rows carry the node owner the redaction and fence read.
	if _, err := db.Exec(`UPDATE nodes SET owner_id = $1 WHERE id = $2`, f.user.ID, f.node.ID); err != nil {
		t.Fatalf("set node owner: %v", err)
	}
	t.Cleanup(func() { db.Exec(`UPDATE nodes SET owner_id = NULL WHERE id = $1`, f.node.ID) })
	list, err := st.ListServersForUser(f.user.ID, false)
	if err != nil {
		t.Fatalf("ListServersForUser: %v", err)
	}
	found := false
	for _, s := range list {
		if s.ID == f.server.ID {
			found = true
			if !s.NodeOwnerLoaded || s.NodeOwnerID == nil || *s.NodeOwnerID != f.user.ID {
				t.Errorf("list row node owner loaded=%v owner=%v", s.NodeOwnerLoaded, s.NodeOwnerID)
			}
		}
	}
	if !found {
		t.Fatal("the owner's own server is not in their list")
	}
}
