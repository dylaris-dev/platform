package database

import (
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// TestIntegrationServerListMatchesTheResolver pins the server list against the
// resolver on a real Postgres. The resolver on the same rows is the oracle, not
// expectations written by hand, so a grant shape the list forgets fails here
// without anyone having to think of it.
//
// The list is built in two stages, and each has its own promise:
//
//   - ListServersForUser (SQL) must never MISS a server the resolver opens. A
//     missed server cannot be put back later.
//   - The panel then drops every invited or inherited row the resolver gives no
//     server capability on (handlers.applyResolvedTabPermissions, via
//     Resolution.HasAnyServerCap). SQL cannot do that: which scope a capability
//     belongs to lives in the Go catalog. The final answer is therefore asserted
//     with that same rule applied here.
//
// The old query was wrong three ways, each with a row below that tells it apart:
// account-wide grants were never listed, inheritance was read off the legacy
// blob the grants path never writes, and inheritance crossed owners where the
// resolver refuses. A fourth row is the case an adversarial review found in the
// first version of this change: an account-wide grant carrying only OWNER caps
// matches every server of that owner and opens none of them.
func TestIntegrationServerListMatchesTheResolver(t *testing.T) {
	_, st := integrationDB(t)

	user := func(p string) *models.User {
		u := &models.User{Username: uniqueName(p), Password: "x", Email: uniqueName(p) + "@example.test"}
		if err := st.CreateUser(u); err != nil {
			t.Fatalf("CreateUser(%s): %v", p, err)
		}
		t.Cleanup(func() { st.DeleteUser(u.ID) })
		return u
	}
	friend := user("lf_")
	ownerA, ownerB, ownerC, ownerD, ownerE, ownerG := user("la_"), user("lb_"), user("lc_"), user("ld_"), user("le_"), user("lg_")

	node := &models.Node{Name: uniqueName("ln_"), Address: "127.0.0.1", Token: uniqueName("lt_"), Status: "online"}
	if err := st.CreateNode(node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	var created []*models.Server
	// candidateOnly are rows SQL lists and the resolver refuses. Declared, so every
	// OTHER row must match the resolver exactly at the SQL stage too.
	candidateOnly := map[int]bool{}
	server := func(name string, owner *models.User, proxy *models.Server) *models.Server {
		s := &models.Server{
			UUID: uniqueName("lu_"), Name: uniqueName(name), NodeID: node.ID, OwnerID: owner.ID,
			GameImage: "img", Port: 25600, Memory: 1024, Status: "stopped", ServerType: "game",
		}
		if proxy != nil {
			pid := proxy.ID
			s.ProxyID = &pid
		}
		id, err := st.CreateServer(s)
		if err != nil {
			t.Fatalf("CreateServer(%s): %v", name, err)
		}
		s.ID = int(id)
		created = append(created, s)
		return s
	}
	// Servers are removed before the node and users, so the foreign keys let go.
	t.Cleanup(func() {
		for i := len(created) - 1; i >= 0; i-- {
			st.DeleteServer(created[i].ID)
		}
		st.DeleteNode(node.ID)
	})

	console := store.CapOverrides{Grant: []string{"console.read"}}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// A direct invite through the legacy path. Listed before and after.
	invited := server("l_invited_", ownerA, nil)
	must("CreateInvite", st.CreateInvite(invited.ID, friend.ID, ownerA.ID, map[string]bool{"console": true}))

	// Owned by A and not shared at all. Never listed.
	server("l_private_", ownerA, nil)

	// An account-wide grant from B covers every server B has. The old query had
	// no arm for server_id IS NULL, so neither of these was listed.
	server("l_acct1_", ownerB, nil)
	server("l_acct2_", ownerB, nil)
	must("account-wide grant", st.UpsertServerGrant(nil, friend.ID, ownerB.ID, nil, console, false))

	// An account-wide grant from G carrying only an OWNER cap. It reaches G's
	// modpacks and no server. SQL lists it (a row exists), the resolver opens
	// nothing, and the panel must drop it.
	ownOnly := server("l_ownonly_", ownerG, nil)
	candidateOnly[ownOnly.ID] = true
	must("owner-only account-wide grant", st.UpsertServerGrant(nil, friend.ID, ownerG.ID, nil, store.CapOverrides{Grant: []string{"modpack.read"}}, false))

	// Inheritance granted through the GRANTS path, which writes the inherit
	// column and never the legacy blob. The old query read the blob, so the child
	// was not listed.
	proxyA := server("l_proxyA_", ownerA, nil)
	server("l_childA_", ownerA, proxyA)
	pid := proxyA.ID
	must("inheriting grant", st.UpsertServerGrant(&pid, friend.ID, ownerA.ID, nil, console, true))

	// A child of A's proxy that belongs to somebody ELSE. Hidden - but on its own
	// this row cannot prove the owner check: the grant on proxyA came through the
	// grants path and has no blob, so the old query skipped it for THAT reason.
	server("l_crossC_", ownerC, proxyA)

	// The row that does prove it. A LEGACY invite with inheritance on writes the
	// blob AND the column, so the old query's blob test passed and only the
	// missing owner check let a child of somebody else through. The resolver
	// refuses it ("the proxy must belong to the SAME owner as the child").
	proxyE := server("l_proxyE_", ownerE, nil)
	server("l_childE_", ownerE, proxyE)
	server("l_crossE_", ownerC, proxyE)
	must("legacy inheriting invite on E", st.CreateInvite(proxyE.ID, friend.ID, ownerE.ID, map[string]bool{"console": true, "inherit": true}))

	// Inheritance switched OFF through the grants path on an invite that was made
	// with it ON through the legacy path. The blob still says true, the column
	// says false; the old query believed the blob and kept listing the child.
	proxyD := server("l_proxyD_", ownerD, nil)
	server("l_childD_", ownerD, proxyD)
	must("legacy inheriting invite", st.CreateInvite(proxyD.ID, friend.ID, ownerD.ID, map[string]bool{"console": true, "inherit": true}))
	pidD := proxyD.ID
	must("switch inheritance off", st.UpsertServerGrant(&pidD, friend.ID, ownerD.ID, nil, console, false))

	listed, err := st.ListServersForUser(friend.ID, false)
	must("ListServersForUser", err)
	inList := map[int]string{}
	for _, s := range listed {
		// UNION ALL across four arms, de-duplicated only by NOT EXISTS clauses. A
		// map would silently swallow a server listed twice.
		if prev, dup := inList[s.ID]; dup {
			t.Errorf("%s is listed twice (roles %q and %q)", s.Name, prev, s.Role)
		}
		inList[s.ID] = s.Role
	}

	resolver := authz.NewResolver(st)
	identity := authz.Identity{UserID: friend.ID, Username: friend.Username}
	opens := 0
	for _, s := range created {
		res, err := resolver.Resolve(identity, s.ID)
		must("Resolve", err)
		may := res.HasAnyServerCap()
		if may {
			opens++
		}
		role, listedBySQL := inList[s.ID]

		// Stage 1: SQL never misses what the resolver opens.
		if may && !listedBySQL {
			t.Errorf("%s: the resolver opens it and the query did not list it - nothing downstream can put it back", s.Name)
		}
		// SQL is exact everywhere it can be; the declared rows are the only superset.
		if !may && listedBySQL && !candidateOnly[s.ID] {
			t.Errorf("%s: listed by the query (role %q) although the resolver refuses it", s.Name, role)
		}
		if candidateOnly[s.ID] && (!listedBySQL || may) {
			t.Errorf("%s: fixture assumption broken - expected the query to list it and the resolver to refuse it (listed=%v, may=%v)", s.Name, listedBySQL, may)
		}

		// Stage 2: the panel's answer, with the handler's rule applied.
		shown := listedBySQL && (role == "owner" || may)
		if shown != may {
			t.Errorf("%s: the panel would show=%v, the resolver says may=%v", s.Name, shown, may)
		}
	}

	// The oracle only works if the fixture holds both answers. A resolver that
	// refused everything would agree with an empty list.
	if opens != 8 || len(inList) != 9 {
		t.Fatalf("fixture shape: resolver opens %d, query lists %d; want 8 opened (invited, acct1, acct2, proxyA, childA, proxyD, proxyE, childE) and 9 listed (those plus ownonly)", opens, len(inList))
	}
}

// TestIntegrationAdminListReachesACustomersServerOnlyByANamedInvite covers the
// ADMIN branch, where neither the resolver nor the panel's filter can help: the
// resolver grants an admin everything, so it would agree with any list.
//
// On a machine somebody owns, an operator is listed a server only through an
// invite naming THAT server. An account-wide grant is deliberately not enough -
// it may carry nothing but owner caps, and would then put every server the
// customer runs on their own hardware into the admin's list.
func TestIntegrationAdminListReachesACustomersServerOnlyByANamedInvite(t *testing.T) {
	_, st := integrationDB(t)

	user := func(p string, admin bool) *models.User {
		u := &models.User{Username: uniqueName(p), Password: "x", Email: uniqueName(p) + "@example.test", IsAdmin: admin}
		if err := st.CreateUser(u); err != nil {
			t.Fatalf("CreateUser(%s): %v", p, err)
		}
		t.Cleanup(func() { st.DeleteUser(u.ID) })
		return u
	}
	admin := user("aa_", true)
	customer, stranger := user("ac_", false), user("as_", false)

	node := &models.Node{Name: uniqueName("an_"), Address: "127.0.0.1", Token: uniqueName("at_"), Status: "online"}
	if err := st.CreateNode(node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := st.SetNodeOwner(node.ID, &customer.ID); err != nil {
		t.Fatalf("SetNodeOwner: %v", err)
	}

	var created []int
	server := func(name string, owner *models.User) int {
		s := &models.Server{
			UUID: uniqueName("au_"), Name: uniqueName(name), NodeID: node.ID, OwnerID: owner.ID,
			GameImage: "img", Port: 25600, Memory: 1024, Status: "stopped", ServerType: "game",
		}
		id, err := st.CreateServer(s)
		if err != nil {
			t.Fatalf("CreateServer(%s): %v", name, err)
		}
		created = append(created, int(id))
		return int(id)
	}
	t.Cleanup(func() {
		for _, id := range created {
			st.DeleteServer(id)
		}
		st.DeleteNode(node.ID)
	})

	named := server("a_named_", customer)
	onlyAccountWide := server("a_acct_", customer)
	strangers := server("a_stranger_", stranger)
	if err := st.CreateInvite(named, admin.ID, customer.ID, map[string]bool{"console": true}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := st.UpsertServerGrant(nil, admin.ID, customer.ID, nil, store.CapOverrides{Grant: []string{"modpack.read"}}, false); err != nil {
		t.Fatalf("account-wide grant: %v", err)
	}

	listed, err := st.ListServersForUser(admin.ID, true)
	if err != nil {
		t.Fatalf("ListServersForUser: %v", err)
	}
	seen := map[int]bool{}
	for _, s := range listed {
		seen[s.ID] = true
	}
	if !seen[named] {
		t.Errorf("a server on a customer's machine the admin was invited to by name is missing from the admin list")
	}
	if seen[onlyAccountWide] {
		t.Errorf("an account-wide grant put a server on a customer's machine into the admin list")
	}
	if seen[strangers] {
		t.Errorf("a server on a customer's machine that nobody let the admin into is in the admin list")
	}
}
