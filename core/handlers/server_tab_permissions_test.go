package handlers

import (
	"database/sql"
	"errors"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

func capSet(ids ...string) func(string) bool {
	held := make(map[string]bool, len(ids))
	for _, id := range ids {
		held[id] = true
	}
	return func(c string) bool { return held[c] }
}

// A grant made through the Access page writes cap_overrides, never the legacy
// invite blob the panel gates its tabs on, so a member who reached the server
// through the cap model saw every tab locked while the API served them fine.
func TestMergeResolvedTabPermissions(t *testing.T) {
	tests := []struct {
		name string
		base *models.TabPermissions
		caps func(string) bool
		want models.TabPermissions
	}{
		{
			name: "no legacy invite, caps alone light the tabs",
			base: nil,
			caps: capSet("overview.read", "backups.read"),
			want: models.TabPermissions{Overview: true, Backups: true},
		},
		{
			name: "legacy bits survive when the cap set is empty",
			base: &models.TabPermissions{Console: true, Inherit: true},
			caps: capSet(),
			want: models.TabPermissions{Console: true, Inherit: true},
		},
		{
			name: "caps are OR-ed onto the legacy bits, never subtracted",
			base: &models.TabPermissions{Console: true},
			caps: capSet("files.read"),
			want: models.TabPermissions{Console: true, Files: true},
		},
		{
			name: "inherit is an invite column, no capability sets it",
			base: nil,
			caps: capSet("overview.read", "console.read", "files.read", "config.read",
				"power.start", "network.read", "members.read", "backups.read", "server.settings.write"),
			want: models.TabPermissions{
				Console: true, Files: true, Config: true, Setup: true, Overview: true,
				Power: true, Members: true, Network: true, Backups: true,
			},
		},
		{
			name: "a nil resolver leaves the blob untouched",
			base: &models.TabPermissions{Overview: true},
			caps: nil,
			want: models.TabPermissions{Overview: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeResolvedTabPermissions(tt.base, tt.caps); got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Each tab bit is spelled with a real capability id. A typo here would silently
// leave that tab locked forever, which is the failure this whole mapping exists
// to fix.
func TestTabPermissionCapsAreRealCapabilities(t *testing.T) {
	for _, m := range tabPermissionCaps {
		c, ok := authz.Get(m.cap)
		if !ok {
			t.Errorf("%q is not in the capability catalog", m.cap)
			continue
		}
		if c.Scope != authz.ScopeServer {
			t.Errorf("%q has scope %v, want ScopeServer - a tab is a per-server thing", m.cap, c.Scope)
		}
	}
}

// tabListAuthzStore is the resolver's store port, faked: grants per server id and
// per owner, nothing else. Every server is owned by "owner-1".
type tabListAuthzStore struct {
	grants  map[int]*store.ServerGrant
	account *store.ServerGrant
	failOn  int
}

func (f tabListAuthzStore) GetServerByID(id int) (*models.Server, error) {
	if id == f.failOn {
		return nil, errors.New("connection refused")
	}
	return &models.Server{ID: id, OwnerID: "owner-1"}, nil
}
func (f tabListAuthzStore) GetServerByUUID(string) (*models.Server, error) { return nil, sql.ErrNoRows }
func (f tabListAuthzStore) GetPanelRole(int) (*store.PanelRole, error)     { return nil, sql.ErrNoRows }
func (f tabListAuthzStore) GetServerRole(int) (*store.ServerRole, error)   { return nil, sql.ErrNoRows }
func (f tabListAuthzStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f tabListAuthzStore) GetServerGrant(serverID int, _ string) (*store.ServerGrant, error) {
	if g, ok := f.grants[serverID]; ok {
		return g, nil
	}
	return nil, sql.ErrNoRows
}
func (f tabListAuthzStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	if f.account == nil {
		return nil, sql.ErrNoRows
	}
	return f.account, nil
}

// A grant ROW is not a permission. The list query selects every server a grant
// points at; an account-wide grant holding only OWNER caps points at every server
// its owner has and opens none of them. Before, each of those servers reached the
// member's list - node address, ports, start command - with a 403 behind it.
func TestServerListDropsInvitedServersTheResolverWillNotOpen(t *testing.T) {
	st := tabListAuthzStore{
		grants: map[int]*store.ServerGrant{
			1: {CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
			// Every cap revoked, the row still there.
			2: {CapOverrides: store.CapOverrides{}},
		},
		// Owner tools only: modpacks. No server capability at all.
		account: &store.ServerGrant{CapOverrides: store.CapOverrides{Grant: []string{"modpack.read"}}},
		failOn:  4,
	}
	state := &AppState{Authz: authz.NewResolver(st)}

	servers := []models.Server{
		{ID: 1, Role: "invited"},
		{ID: 2, Role: "invited"},
		{ID: 3, Role: "invited"}, // reached only through the owner-caps account grant
		{ID: 4, Role: "inherited"},
		{ID: 5, Role: "owner"}, // the caller's own: never resolved, never dropped
	}
	got := applyResolvedTabPermissions(state, servers, "friend-1", "friend")

	kept := map[int]bool{}
	for _, s := range got {
		kept[s.ID] = true
	}
	if !kept[1] {
		t.Error("server 1 carries console.read and was dropped")
	}
	if kept[2] {
		t.Error("server 2 has a grant row with no capability and is still listed")
	}
	if kept[3] {
		t.Error("server 3 is reachable only through an account-wide grant of owner caps and is still listed")
	}
	// A server the store cannot load resolves to nothing (Resolve denies rather
	// than erroring), and nothing is not "may": this decides membership.
	if kept[4] {
		t.Error("server 4 could not be loaded and is still listed")
	}
	if !kept[5] {
		t.Error("the caller's own server was dropped")
	}
	if len(got) != 2 {
		t.Fatalf("kept %d servers, want 2: %+v", len(got), got)
	}
}
