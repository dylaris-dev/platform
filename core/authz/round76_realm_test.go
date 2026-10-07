package authz

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// An admin owner change moves the server and leaves its grant rows behind,
// still in the previous owner's realm. Those rows kept working: the previous
// owner's friends kept their access, and the previous owner could widen it by
// editing the server role the rows point at.
func TestResolve_GrantFromAPreviousOwnersRealmIsInert(t *testing.T) {
	proxyID := 9
	fs := &resolverFakeStore{
		servers: map[int]*models.Server{
			5: {ID: 5, OwnerID: "new-owner", OwnerName: "new-owner", ProxyID: &proxyID},
			9: {ID: 9, OwnerID: "new-owner", OwnerName: "new-owner"},
		},
		serverRoles: map[int]*store.ServerRole{3: {ID: 3, Capabilities: []string{"console.read"}}},
		serverGrants: map[string]*store.ServerGrant{
			gkey(5, "old-friend"):   {UserID: "old-friend", OwnerUserID: "old-owner", ServerRoleID: intp(3)},
			gkey(5, "new-friend"):   {UserID: "new-friend", OwnerUserID: "new-owner", ServerRoleID: intp(3)},
			gkey(9, "proxy-friend"): {UserID: "proxy-friend", OwnerUserID: "old-owner", ServerRoleID: intp(3), Inherit: true},
		},
	}
	r := NewResolver(fs)
	for user, want := range map[string]bool{"old-friend": false, "new-friend": true, "proxy-friend": false} {
		res, _ := r.Resolve(Identity{UserID: user}, 5)
		if got := res.HasCap("console.read"); got != want {
			t.Errorf("%s: console.read = %v, want %v", user, got, want)
		}
	}
}

// Editing a stale member row moves it into the new owner's realm but keeps its
// role id; the previous owner's role must not come with it.
func TestResolve_RoleFromAnotherRealmIsInert(t *testing.T) {
	fs := &resolverFakeStore{
		servers: map[int]*models.Server{5: {ID: 5, OwnerID: "new-owner", OwnerName: "new-owner"}},
		serverRoles: map[int]*store.ServerRole{
			3: {ID: 3, OwnerUserID: "old-owner", Capabilities: []string{"files.write"}},
			4: {ID: 4, OwnerUserID: "new-owner", Capabilities: []string{"console.read"}},
		},
		serverGrants: map[string]*store.ServerGrant{
			gkey(5, "edited"): {UserID: "edited", OwnerUserID: "new-owner", ServerRoleID: intp(3)},
			gkey(5, "normal"): {UserID: "normal", OwnerUserID: "new-owner", ServerRoleID: intp(4)},
		},
	}
	r := NewResolver(fs)
	if res, _ := r.Resolve(Identity{UserID: "edited"}, 5); res.HasCap("files.write") {
		t.Error("a role of the previous owner still applies on an edited row")
	}
	if res, _ := r.Resolve(Identity{UserID: "normal"}, 5); !res.HasCap("console.read") {
		t.Error("the owner's own role stopped applying")
	}
}
