package authz

import (
	"context"
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// resolverFakeStore implements the narrow authz.Store surface. Only the fields
// a case sets are consulted; anything absent returns a "not found" error, which
// the resolver treats as deny-by-default.
type resolverFakeStore struct {
	servers       map[int]*models.Server
	serversByUUID map[string]*models.Server
	panelRoles    map[int]*store.PanelRole
	serverRoles   map[int]*store.ServerRole
	panelAuthz    map[string]struct {
		roleID    *int
		overrides store.CapOverrides
		err       error
	}
	serverGrants  map[string]*store.ServerGrant // key: fmt serverID|userID
	accountGrants map[string]*store.ServerGrant // key: ownerUserID|userID
}

func (f *resolverFakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return nil, errors.New("no server")
}
func (f *resolverFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	if s, ok := f.serversByUUID[uuid]; ok {
		return s, nil
	}
	return nil, errors.New("no server")
}
func (f *resolverFakeStore) GetPanelRole(id int) (*store.PanelRole, error) {
	if r, ok := f.panelRoles[id]; ok {
		return r, nil
	}
	return nil, errors.New("no role")
}
func (f *resolverFakeStore) GetServerRole(id int) (*store.ServerRole, error) {
	if r, ok := f.serverRoles[id]; ok {
		return r, nil
	}
	return nil, errors.New("no role")
}
func (f *resolverFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	if v, ok := f.panelAuthz[userID]; ok {
		return v.roleID, v.overrides, v.err
	}
	return nil, store.CapOverrides{}, nil
}
func (f *resolverFakeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if g, ok := f.serverGrants[gkey(serverID, userID)]; ok {
		return g, nil
	}
	return nil, errors.New("no grant")
}
func (f *resolverFakeStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	if g, ok := f.accountGrants[ownerUserID+"|"+userID]; ok {
		return g, nil
	}
	return nil, errors.New("no grant")
}

func gkey(serverID int, userID string) string {
	return string(rune(serverID)) + "|" + userID
}
func intp(i int) *int { return &i }

func TestResolve_AdminShortCircuit(t *testing.T) {
	r := NewResolver(&resolverFakeStore{})
	res, err := r.Resolve(Identity{UserID: "u1", IsAdmin: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Admin holds every cap in every scope.
	for _, id := range []string{"users.write", "files.delete", "modpack.write"} {
		if !res.HasCap(id) {
			t.Errorf("admin should hold %q", id)
		}
	}
}

func TestResolve_DenyByDefault(t *testing.T) {
	r := NewResolver(&resolverFakeStore{})
	res, _ := r.Resolve(Identity{UserID: "u1"}, 0)
	// Ordinary user with no panel role holds no PANEL caps.
	if res.HasCap("users.read") {
		t.Error("ordinary user must not hold users.read")
	}
	// Unknown cap is always deny.
	if res.HasCap("does.not.exist") {
		t.Error("unknown cap must be denied")
	}
}

func TestResolve_OwnerSelfRealm(t *testing.T) {
	r := NewResolver(&resolverFakeStore{})
	res, _ := r.Resolve(Identity{UserID: "u1"}, 0)
	// Every user owns their own OWNER realm (their own modpacks/library).
	if !res.HasCap("modpack.read") {
		t.Error("user should hold owner-scope caps on their own realm")
	}
}

func TestResolve_PanelRolePlusOverrides(t *testing.T) {
	fs := &resolverFakeStore{
		panelRoles: map[int]*store.PanelRole{7: {ID: 7, Capabilities: []string{"users.read", "users.write"}}},
		panelAuthz: map[string]struct {
			roleID    *int
			overrides store.CapOverrides
			err       error
		}{
			"u1": {roleID: intp(7), overrides: store.CapOverrides{Grant: []string{"nodes.read"}, Deny: []string{"users.write"}}},
		},
	}
	r := NewResolver(fs)
	res, _ := r.Resolve(Identity{UserID: "u1"}, 0)
	if !res.HasCap("users.read") {
		t.Error("role cap users.read expected")
	}
	if res.HasCap("users.write") {
		t.Error("override deny should remove users.write")
	}
	if !res.HasCap("nodes.read") {
		t.Error("override grant should add nodes.read")
	}
}

func TestResolve_ServerOwnerShortCircuit(t *testing.T) {
	fs := &resolverFakeStore{servers: map[int]*models.Server{5: {ID: 5, OwnerID: "u1", OwnerName: "alice"}}}
	r := NewResolver(fs)
	res, _ := r.Resolve(Identity{UserID: "u1", Username: "alice"}, 5)
	if !res.HasCap("files.delete") {
		t.Error("server owner should hold every SERVER cap")
	}
}

func TestResolve_InvitedFriendServerRole(t *testing.T) {
	fs := &resolverFakeStore{
		servers:     map[int]*models.Server{5: {ID: 5, OwnerID: "owner", OwnerName: "owner"}},
		serverRoles: map[int]*store.ServerRole{3: {ID: 3, Capabilities: []string{"files.read"}}},
		serverGrants: map[string]*store.ServerGrant{
			gkey(5, "friend"): {UserID: "friend", ServerRoleID: intp(3)},
		},
	}
	r := NewResolver(fs)
	res, _ := r.Resolve(Identity{UserID: "friend", Username: "bob"}, 5)
	if !res.HasCap("files.read") {
		t.Error("invited friend should hold files.read from server-role")
	}
	if res.HasCap("files.write") {
		t.Error("invited friend must not hold ungranted files.write")
	}
}

func TestResolve_ProxyInherit(t *testing.T) {
	proxyID := 9
	fs := &resolverFakeStore{
		servers: map[int]*models.Server{
			5: {ID: 5, OwnerID: "owner", OwnerName: "owner", ProxyID: &proxyID},
			9: {ID: 9, OwnerID: "owner", OwnerName: "owner"},
		},
		serverRoles: map[int]*store.ServerRole{3: {ID: 3, Capabilities: []string{"console.read"}}},
		serverGrants: map[string]*store.ServerGrant{
			gkey(9, "friend"): {UserID: "friend", ServerRoleID: intp(3), Inherit: true},
		},
	}
	r := NewResolver(fs)
	res, _ := r.Resolve(Identity{UserID: "friend"}, 5) // no direct grant on 5
	if !res.HasCap("console.read") {
		t.Error("proxy invite with inherit=true should grant console.read on the child")
	}
}

func TestResolve_AccountGrantOwnerScope(t *testing.T) {
	fs := &resolverFakeStore{
		servers:     map[int]*models.Server{5: {ID: 5, OwnerID: "owner", OwnerName: "owner"}},
		serverRoles: map[int]*store.ServerRole{4: {ID: 4, Capabilities: []string{"modpack.write", "files.read"}}},
		accountGrants: map[string]*store.ServerGrant{
			"owner|friend": {UserID: "friend", OwnerUserID: "owner", ServerRoleID: intp(4)},
		},
	}
	r := NewResolver(fs)
	res, _ := r.Resolve(Identity{UserID: "friend"}, 5)
	if !res.HasCap("modpack.write") {
		t.Error("account-wide grant should give owner-scope modpack.write")
	}
	if !res.HasCap("files.read") {
		t.Error("account-wide grant should give server-scope files.read")
	}
}

func TestIdentityFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), "userID", "u1")
	ctx = context.WithValue(ctx, "username", "alice")
	ctx = context.WithValue(ctx, "isAdmin", true)
	id := IdentityFromContext(ctx)
	if id.UserID != "u1" || id.Username != "alice" || !id.IsAdmin {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

func TestResolve_ProxyInheritRejectsCrossOwner(t *testing.T) {
	proxyID := 9
	fs := &resolverFakeStore{
		servers: map[int]*models.Server{
			5: {ID: 5, OwnerID: "ownerA", OwnerName: "ownerA", ProxyID: &proxyID},
			9: {ID: 9, OwnerID: "ownerB", OwnerName: "ownerB"}, // proxy owned by a DIFFERENT owner
		},
		serverRoles: map[int]*store.ServerRole{3: {ID: 3, Capabilities: []string{"console.read"}}},
		serverGrants: map[string]*store.ServerGrant{
			gkey(9, "friend"): {UserID: "friend", ServerRoleID: intp(3), Inherit: true},
		},
	}
	r := NewResolver(fs)
	res, _ := r.Resolve(Identity{UserID: "friend"}, 5)
	if res.HasCap("console.read") {
		t.Error("proxy-inherit must NOT cross owners: child ownerA, proxy ownerB")
	}
}

func TestResolve_DemoReadGrantsServerReadCapsOnly(t *testing.T) {
	fs := &resolverFakeStore{servers: map[int]*models.Server{5: {ID: 5, OwnerID: "owner", OwnerName: "owner"}}}
	r := NewResolver(fs)
	r.SetDemoRead(func(serverID int) bool { return serverID == 5 })
	res, _ := r.Resolve(Identity{UserID: "stranger"}, 5)
	for _, id := range []string{"overview.read", "console.read", "stats.read"} {
		if !res.HasCap(id) {
			t.Errorf("demo server should grant read cap %q to any authed user", id)
		}
	}
	for _, id := range []string{"console.send", "files.write", "files.delete", "power.start", "backups.create"} {
		if res.HasCap(id) {
			t.Errorf("demo server must NOT grant non-read cap %q", id)
		}
	}
	// Topology + roster disclosure stays denied on a public demo even though these
	// are read caps (network.read = routing/endpoint, members.read = access list).
	// files.read is also denied: the file browser handler owns demo file access
	// itself (via viaDemoBypass) so it can redact secrets; if the resolver granted
	// files.read here, the handler would skip its own redaction path.
	// server.audit.read is the sharpest of the four: the audit rows carry the IP
	// address and user agent of the owner and of every member.
	// backups.read hands out a download of every archive: the whole world,
	// unredacted, the thing files.read is withheld for.
	for _, id := range []string{"network.read", "members.read", "files.read", "server.audit.read", "backups.read"} {
		if res.HasCap(id) {
			t.Errorf("demo server must NOT grant sensitive read cap %q to a stranger", id)
		}
	}
}

// demoReadReviewed is every server read cap a public demo GRANTS, each one
// looked at. A new server read cap is granted to every signed-in account the
// moment it exists, so it must be put here or on demoReadDeny deliberately.
var demoReadReviewed = map[string]string{
	"overview.read": "status and resources",
	"console.read":  "the showcase itself",
	"players.read":  "who is online",
	"config.read":   "RCON port and whether a secret is set, never the secret",
	"mods.read":     "installed mods",
	"tabs.read":     "the operator's custom tabs, meant to be shown",
	"schedule.read": "scheduled tasks",
	"stats.read":    "graphs",
}

func TestEveryServerReadCapIsClassifiedForTheDemo(t *testing.T) {
	for _, c := range All() {
		if c.Scope != ScopeServer || c.Verb != VerbRead {
			continue
		}
		_, granted := demoReadReviewed[c.ID]
		if granted == demoReadDeny[c.ID] {
			t.Errorf("%q must be on exactly one of demoReadReviewed (granted to every account on a demo server) or demoReadDeny", c.ID)
		}
	}
}

// TestDemoReadDenyEntriesAreRealServerReadCaps keeps the deny list from going
// silently inert. Its entries are bare strings matched against a cap id, and
// the short-circuit they guard is "grant every ScopeServer read cap that is not
// on this list" - so a typo, a renamed cap or a wrong-scope entry does not fail
// anywhere, it just quietly starts granting the thing the entry was written to
// withhold.
func TestDemoReadDenyEntriesAreRealServerReadCaps(t *testing.T) {
	for id := range demoReadDeny {
		c, ok := Get(id)
		if !ok {
			t.Errorf("demoReadDeny lists %q, which is not a catalog capability: the entry withholds nothing", id)
			continue
		}
		if c.Scope != ScopeServer || c.Verb != VerbRead {
			t.Errorf("demoReadDeny lists %q (scope %q, verb %q); the demo short-circuit only ever grants server/read caps, so this entry is dead weight", id, c.Scope, c.Verb)
		}
	}
}

func TestResolve_DemoReadOffByDefault(t *testing.T) {
	fs := &resolverFakeStore{servers: map[int]*models.Server{5: {ID: 5, OwnerID: "owner", OwnerName: "owner"}}}
	r := NewResolver(fs) // no SetDemoRead
	res, _ := r.Resolve(Identity{UserID: "stranger"}, 5)
	if res.HasCap("overview.read") {
		t.Error("without a demo predicate a stranger holds no server caps")
	}
}

func TestCapSubset_Delegation(t *testing.T) {
	// A non-owner assigner can only grant caps they themselves hold.
	fs := &resolverFakeStore{
		servers:     map[int]*models.Server{5: {ID: 5, OwnerID: "owner", OwnerName: "owner"}},
		serverRoles: map[int]*store.ServerRole{3: {ID: 3, Capabilities: []string{"files.read"}}},
		serverGrants: map[string]*store.ServerGrant{
			gkey(5, "friend"): {UserID: "friend", ServerRoleID: intp(3)},
		},
	}
	r := NewResolver(fs)
	res, _ := r.Resolve(Identity{UserID: "friend"}, 5)
	got := CapSubset(res, []string{"files.read", "files.write"})
	if len(got) != 1 || got[0] != "files.read" {
		t.Fatalf("CapSubset = %v, want [files.read]", got)
	}

	// Admin holds everything, so nothing is capped away.
	adminRes, _ := r.Resolve(Identity{UserID: "root", IsAdmin: true}, 5)
	full := CapSubset(adminRes, []string{"files.read", "files.write"})
	if len(full) != 2 {
		t.Fatalf("admin CapSubset = %v, want both caps", full)
	}
}
