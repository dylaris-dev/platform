package authz

import (
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// countingStore counts every resolver read, so a test can pin how many
// statements one decision costs.
type countingStore struct {
	*resolverFakeStore
	calls map[string]int
}

func newCounting(f *resolverFakeStore) *countingStore {
	return &countingStore{resolverFakeStore: f, calls: map[string]int{}}
}

func (c *countingStore) total() int {
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *countingStore) GetServerByID(id int) (*models.Server, error) {
	c.calls["GetServerByID"]++
	return c.resolverFakeStore.GetServerByID(id)
}
func (c *countingStore) GetServerByUUID(uuid string) (*models.Server, error) {
	c.calls["GetServerByUUID"]++
	return c.resolverFakeStore.GetServerByUUID(uuid)
}
func (c *countingStore) GetPanelRole(id int) (*store.PanelRole, error) {
	c.calls["GetPanelRole"]++
	return c.resolverFakeStore.GetPanelRole(id)
}
func (c *countingStore) GetServerRole(id int) (*store.ServerRole, error) {
	c.calls["GetServerRole"]++
	return c.resolverFakeStore.GetServerRole(id)
}
func (c *countingStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	c.calls["GetUserPanelAuthz"]++
	return c.resolverFakeStore.GetUserPanelAuthz(userID)
}
func (c *countingStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	c.calls["GetServerGrant"]++
	return c.resolverFakeStore.GetServerGrant(serverID, userID)
}
func (c *countingStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	c.calls["GetAccountGrant"]++
	return c.resolverFakeStore.GetAccountGrant(ownerUserID, userID)
}

// An admin on a server costs one read: the row carries its node's owner, and
// the foreign-node predicate is handed that row instead of a node id to look
// up. The fence still closes on a customer's machine.
func TestResolveAdminReadsTheServerOnce(t *testing.T) {
	owner := "customer"
	cs := newCounting(&resolverFakeStore{servers: map[int]*models.Server{
		1: {ID: 1, OwnerID: owner, NodeID: 10, NodeOwnerLoaded: true},
		2: {ID: 2, OwnerID: owner, NodeID: 20, NodeOwnerID: &owner, NodeOwnerLoaded: true},
	}})
	r := NewResolver(cs)
	var seen *models.Server
	r.SetForeignNode(func(srv *models.Server, userID string) bool {
		seen = srv
		return srv.NodeOwnerID != nil && *srv.NodeOwnerID != userID
	})
	admin := Identity{UserID: "admin", IsAdmin: true}

	res, _ := r.Resolve(admin, 1)
	if !res.HasCap("files.write") || cs.total() != 1 || seen == nil || seen.ID != 1 {
		t.Fatalf("platform server: files.write=%v reads=%v seen=%+v", res.HasCap("files.write"), cs.calls, seen)
	}
	res, _ = r.Resolve(admin, 2)
	if res.HasCap("files.write") {
		t.Fatal("an admin holds files.write on a customer's machine")
	}
}

// The demo predicate gets the UUID of the row the resolver already loaded, so
// a demo viewer costs no second server read; a non-demo server still denies.
func TestResolveDemoReadUsesTheLoadedRow(t *testing.T) {
	cs := newCounting(&resolverFakeStore{servers: map[int]*models.Server{
		5: {ID: 5, UUID: "demo", OwnerID: "owner"},
		6: {ID: 6, UUID: "plain", OwnerID: "owner"},
	}})
	r := NewResolver(cs)
	r.SetDemoRead(func(uuid string) bool { return uuid == "demo" })
	stranger := Identity{UserID: "stranger"}

	res, _ := r.Resolve(stranger, 5)
	if !res.HasCap("overview.read") || cs.calls["GetServerByID"] != 1 {
		t.Fatalf("demo: overview.read=%v reads=%v", res.HasCap("overview.read"), cs.calls)
	}
	res, _ = r.Resolve(stranger, 6)
	if res.HasCap("overview.read") {
		t.Fatal("a stranger reads a server that is not on the demo list")
	}
}

type panelEntry = struct {
	roleID    *int
	overrides store.CapOverrides
	err       error
}

// The panel role is read only when a PANEL capability is asked, once per
// resolution, and still decides those checks exactly as before.
func TestResolvePanelCapsAreLazy(t *testing.T) {
	staffRole := 7
	sid := 1
	fake := &resolverFakeStore{
		servers:    map[int]*models.Server{1: {ID: 1, OwnerID: "owner"}},
		panelRoles: map[int]*store.PanelRole{7: {ID: 7, Capabilities: []string{"users.read"}}},
		panelAuthz: map[string]panelEntry{
			"staff":  {roleID: &staffRole, overrides: store.CapOverrides{Deny: []string{"users.read"}, Grant: []string{"nodes.read"}}},
			"broken": {err: errors.New("db down")},
		},
		serverGrants: map[string]*store.ServerGrant{
			gkey(1, "staff"): {ServerID: &sid, UserID: "staff", OwnerUserID: "owner", CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
		},
	}

	t.Run("a server check reads no panel role", func(t *testing.T) {
		cs := newCounting(fake)
		res, _ := NewResolver(cs).Resolve(Identity{UserID: "staff"}, 1)
		if !res.HasCap("console.read") || res.HasCap("console.send") {
			t.Fatal("grant not applied as before")
		}
		if cs.calls["GetUserPanelAuthz"]+cs.calls["GetPanelRole"] != 0 {
			t.Fatalf("panel role read for a server check: %v", cs.calls)
		}
	})
	t.Run("panel checks read it once and decide as before", func(t *testing.T) {
		cs := newCounting(fake)
		res, _ := NewResolver(cs).Resolve(Identity{UserID: "staff"}, 0)
		if res.HasCap("users.read") {
			t.Error("a per-user deny no longer removes a role cap")
		}
		if !res.HasCap("nodes.read") {
			t.Error("a per-user grant no longer adds a cap")
		}
		if res.HasCap("settings.write") {
			t.Error("a cap neither role nor override gives was granted")
		}
		if cs.calls["GetUserPanelAuthz"] != 1 || cs.calls["GetPanelRole"] != 1 {
			t.Fatalf("panel reads %v, want one each", cs.calls)
		}
	})
	t.Run("an unreadable panel row denies", func(t *testing.T) {
		res, _ := NewResolver(newCounting(fake)).Resolve(Identity{UserID: "broken"}, 0)
		if res.HasCap("users.read") || res.HasCap("nodes.read") {
			t.Error("a failed panel read granted something")
		}
	})
}

// pairStore is countingStore with the one-statement grant read, answered from
// the same maps the two separate reads use, with roles "joined".
type pairStore struct{ *countingStore }

func (p pairStore) GetServerAndAccountGrants(serverID int, ownerUserID, userID string) (*store.ServerGrant, *store.ServerGrant, error) {
	p.calls["GetServerAndAccountGrants"]++
	join := func(g *store.ServerGrant) *store.ServerGrant {
		if g == nil {
			return nil
		}
		cp := *g
		if cp.ServerRoleID != nil {
			cp.Role = p.serverRoles[*cp.ServerRoleID]
		}
		return &cp
	}
	return join(p.serverGrants[gkey(serverID, userID)]), join(p.accountGrants[ownerUserID+"|"+userID]), nil
}

// A member costs the server read plus ONE grant statement (roles joined),
// and every realm rule still denies what it denied with separate reads.
func TestResolveGrantsInOneStatement(t *testing.T) {
	sid := 1
	viewer, foreignRole := 3, 4
	base := func() *resolverFakeStore {
		return &resolverFakeStore{
			servers: map[int]*models.Server{1: {ID: 1, OwnerID: "owner"}},
			serverRoles: map[int]*store.ServerRole{
				viewer:      {ID: viewer, OwnerUserID: "owner", Capabilities: []string{"console.read"}},
				foreignRole: {ID: foreignRole, OwnerUserID: "someone-else", Capabilities: []string{"files.write"}},
			},
		}
	}
	for _, tc := range []struct {
		name    string
		direct  *store.ServerGrant
		account *store.ServerGrant
		want    map[string]bool
	}{
		{"direct grant with a role", &store.ServerGrant{ServerID: &sid, UserID: "m", OwnerUserID: "owner", ServerRoleID: &viewer},
			nil, map[string]bool{"console.read": true, "files.write": false}},
		{"account grant on the owner's realm", nil,
			&store.ServerGrant{UserID: "m", OwnerUserID: "owner", CapOverrides: store.CapOverrides{Grant: []string{"stats.read"}}},
			map[string]bool{"stats.read": true, "console.read": false}},
		{"both apply", &store.ServerGrant{ServerID: &sid, UserID: "m", OwnerUserID: "owner", ServerRoleID: &viewer},
			&store.ServerGrant{UserID: "m", OwnerUserID: "owner", CapOverrides: store.CapOverrides{Grant: []string{"stats.read"}}},
			map[string]bool{"stats.read": true, "console.read": true}},
		{"a grant left behind by an owner change denies", &store.ServerGrant{ServerID: &sid, UserID: "m", OwnerUserID: "previous-owner", CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
			nil, map[string]bool{"console.read": false}},
		{"a role from another realm grants nothing", &store.ServerGrant{ServerID: &sid, UserID: "m", OwnerUserID: "owner", ServerRoleID: &foreignRole},
			nil, map[string]bool{"files.write": false}},
		{"no grant denies", nil, nil, map[string]bool{"overview.read": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, viaPair := range []bool{false, true} {
				f := base()
				f.serverGrants = map[string]*store.ServerGrant{}
				f.accountGrants = map[string]*store.ServerGrant{}
				if tc.direct != nil {
					f.serverGrants[gkey(1, "m")] = tc.direct
				}
				if tc.account != nil {
					f.accountGrants["owner|m"] = tc.account
				}
				cs := newCounting(f)
				var r *Resolver
				if viaPair {
					r = NewResolver(pairStore{cs})
				} else {
					r = NewResolver(cs)
				}
				res, _ := r.Resolve(Identity{UserID: "m"}, 1)
				for c, want := range tc.want {
					if res.HasCap(c) != want {
						t.Errorf("pair=%v %s = %v, want %v", viaPair, c, !want, want)
					}
				}
				if viaPair && cs.total() != 2 {
					t.Errorf("reads %v, want the server and one grant statement", cs.calls)
				}
			}
		})
	}
}

// listStore adds the one-statement "every grant of this user" read.
type listStore struct {
	*countingStore
}

func (l listStore) ListGrantsForUser(userID string) ([]store.ServerGrant, error) {
	l.calls["ListGrantsForUser"]++
	var out []store.ServerGrant
	add := func(g *store.ServerGrant) {
		if g.UserID != userID {
			return
		}
		cp := *g
		if cp.ServerRoleID != nil {
			cp.Role = l.serverRoles[*cp.ServerRoleID] // joined, as the store does
		}
		out = append(out, cp)
	}
	for _, g := range l.serverGrants {
		add(g)
	}
	for _, g := range l.accountGrants {
		add(g)
	}
	return out, nil
}

// ResolveListed decides every listed server exactly as Resolve does one at a
// time, from one grant read, and every realm rule still denies.
func TestResolveListedMatchesResolve(t *testing.T) {
	ids := func(n int) *int { return &n }
	viewer, foreignRole := 3, 4
	f := &resolverFakeStore{
		servers: map[int]*models.Server{
			1:  {ID: 1, UUID: "a", OwnerID: "owner"},                     // direct grant
			2:  {ID: 2, UUID: "b", OwnerID: "owner"},                     // account grant only
			3:  {ID: 3, UUID: "c", OwnerID: "owner", ProxyID: ids(4)},    // inherits from listed proxy 4
			4:  {ID: 4, UUID: "d", OwnerID: "owner"},                     // proxy with inherit grant
			5:  {ID: 5, UUID: "e", OwnerID: "owner2", ProxyID: ids(6)},   // proxy of ANOTHER owner: no inherit
			6:  {ID: 6, UUID: "f", OwnerID: "owner3"},                    //
			7:  {ID: 7, UUID: "g", OwnerID: "owner2"},                    // stale-realm direct grant
			8:  {ID: 8, UUID: "h", OwnerID: "owner2"},                    // role from another realm
			9:  {ID: 9, UUID: "i", OwnerID: "m"},                         // own server
			10: {ID: 10, UUID: "j", OwnerID: "owner4", ProxyID: ids(11)}, // inherits from an UNLISTED proxy
			11: {ID: 11, UUID: "k", OwnerID: "owner4"},
		},
		serverRoles: map[int]*store.ServerRole{
			viewer:      {ID: viewer, OwnerUserID: "owner", Capabilities: []string{"console.read"}},
			foreignRole: {ID: foreignRole, OwnerUserID: "someone", Capabilities: []string{"files.write"}},
		},
		serverGrants: map[string]*store.ServerGrant{
			gkey(1, "m"):  {ServerID: ids(1), UserID: "m", OwnerUserID: "owner", ServerRoleID: &viewer},
			gkey(4, "m"):  {ServerID: ids(4), UserID: "m", OwnerUserID: "owner", Inherit: true, CapOverrides: store.CapOverrides{Grant: []string{"stats.read"}}},
			gkey(6, "m"):  {ServerID: ids(6), UserID: "m", OwnerUserID: "owner3", Inherit: true, CapOverrides: store.CapOverrides{Grant: []string{"stats.read"}}},
			gkey(7, "m"):  {ServerID: ids(7), UserID: "m", OwnerUserID: "previous", CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
			gkey(8, "m"):  {ServerID: ids(8), UserID: "m", OwnerUserID: "owner2", ServerRoleID: &foreignRole},
			gkey(11, "m"): {ServerID: ids(11), UserID: "m", OwnerUserID: "owner4", Inherit: true, CapOverrides: store.CapOverrides{Grant: []string{"players.read"}}},
			gkey(1, "x"):  {ServerID: ids(1), UserID: "x", OwnerUserID: "owner", CapOverrides: store.CapOverrides{Grant: []string{"files.write"}}},
		},
		accountGrants: map[string]*store.ServerGrant{
			"owner|m": {UserID: "m", OwnerUserID: "owner", CapOverrides: store.CapOverrides{Grant: []string{"overview.read"}}},
		},
	}
	listed := []models.Server{}
	for _, id := range []int{1, 2, 3, 4, 5, 7, 8, 9, 10} { // 6 and 11 not listed
		listed = append(listed, *f.servers[id])
	}
	id := Identity{UserID: "m"}
	caps := []string{"console.read", "stats.read", "overview.read", "files.write", "players.read", "console.send"}

	cs := newCounting(f)
	batch := NewResolver(listStore{countingStore: cs}).ResolveListed(id, listed)
	// One grant list, plus the rows of the two proxies the list did not hold.
	if cs.calls["ListGrantsForUser"] != 1 || cs.calls["GetServerByID"] != 2 || cs.total() != 3 {
		t.Fatalf("batch reads %v, want one grant list and two proxy rows", cs.calls)
	}
	one := NewResolver(f)
	for _, s := range listed {
		want, _ := one.Resolve(id, s.ID)
		got := batch[s.ID]
		if got == nil {
			t.Fatalf("server %d missing from the batch", s.ID)
		}
		if got.HasAnyServerCap() != want.HasAnyServerCap() {
			t.Errorf("server %d: any-cap %v, Resolve says %v", s.ID, got.HasAnyServerCap(), want.HasAnyServerCap())
		}
		for _, c := range caps {
			if got.HasCap(c) != want.HasCap(c) {
				t.Errorf("server %d %s: batch %v, Resolve %v", s.ID, c, got.HasCap(c), want.HasCap(c))
			}
		}
	}
	// Spot-check the denials themselves, not only the agreement.
	for _, c := range []struct {
		sid int
		cap string
	}{{5, "stats.read"}, {7, "console.read"}, {8, "files.write"}, {1, "files.write"}} {
		if batch[c.sid].HasCap(c.cap) {
			t.Errorf("server %d grants %s", c.sid, c.cap)
		}
	}
	if !batch[10].HasCap("players.read") || !batch[3].HasCap("stats.read") || !batch[9].HasCap("files.write") {
		t.Error("an inherited or owned grant was lost")
	}
}

// A list with no shared rows (an owner's, an admin's) reads no grants.
func TestResolveListedEmptyReadsNothing(t *testing.T) {
	cs := newCounting(&resolverFakeStore{})
	if got := NewResolver(listStore{countingStore: cs}).ResolveListed(Identity{UserID: "m"}, nil); len(got) != 0 || cs.total() != 0 {
		t.Fatalf("resolutions %v, reads %v", got, cs.calls)
	}
}
