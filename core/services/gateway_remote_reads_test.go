package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
)

func remoteOn() (bool, bool) { return true, true }

// mintKit gives node a kit on the fake provider and records it locally.
func mintKit(t *testing.T, g *RemoteGateway, nodeID uint) store.RemoteKit {
	t.Helper()
	k, err := g.EnsureKit(context.Background(), nodeID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestActiveRemoteGateway(t *testing.T) {
	rg := &RemoteGateway{}
	for _, tc := range []struct {
		name    string
		mode    func() (bool, bool)
		rg      *RemoteGateway
		wantRG  bool
		wantErr error
	}{
		{"nil mode is local", nil, rg, false, nil},
		{"local", func() (bool, bool) { return false, true }, rg, false, nil},
		{"remote", remoteOn, rg, true, nil},
		{"remote, nothing wired", remoteOn, nil, false, ErrGatewayRemote},
		{"unknown", func() (bool, bool) { return false, false }, rg, false, ErrGatewayProviderUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ActiveRemoteGateway(tc.mode, tc.rg)
			if (got != nil) != tc.wantRG || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

func TestRemoteServerRoutesExcludePendingDeletes(t *testing.T) {
	g, st, _ := newRemoteTest(t)
	seedRemoteRoutes(st)
	st.routes["a2.example.com"] = store.RemoteRoute{Domain: "a2.example.com", ServerUUID: uuidA, NodeID: 7, Port: 25570, ProxyProtocol: true}
	if err := g.DeleteRoute("a1.example.com"); err != nil {
		t.Fatal(err)
	}
	got, err := g.ServerRoutes(uuidA)
	if err != nil {
		t.Fatal(err)
	}
	want := PublicRoute{Domain: "a2.example.com", TargetIP: "mc-" + uuidA, TargetPort: 25570, ServerUUID: uuidA, ProxyProtocol: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ServerRoutes = %+v, want [%+v]", got, want)
	}
	if got, _ := g.ServerRoutes("no-such-server"); got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v, want a non-nil empty slice", got)
	}
}

func TestRemoteRegionsAreCached(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	now := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return now }

	fake.FailNext(1, 503, "http_503", 0)
	if _, err := g.Regions(context.Background()); err == nil {
		t.Fatal("a failed call answered")
	}
	// The failure is served from the cache for 10 s, then read again.
	if _, err := g.Regions(context.Background()); err == nil || countRemoteCalls(fake.Calls(), "GET /regions") != 1 {
		t.Fatalf("failure inside 10 s: err %v, calls %v", err, fake.Calls())
	}
	now = now.Add(11 * time.Second)
	for i := 0; i < 2; i++ {
		r, err := g.Regions(context.Background())
		if err != nil || len(r.HosterDomains) != 1 || r.HosterDomains[0].Domain != "example.com" {
			t.Fatalf("Regions = %+v, %v", r, err)
		}
	}
	if n := countRemoteCalls(fake.Calls(), "GET /regions"); n != 2 {
		t.Fatalf("calls = %d, want 2 (the failure is cached for 10 s, the success for 60 s)", n)
	}
	now = now.Add(61 * time.Second)
	if _, err := g.Regions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countRemoteCalls(fake.Calls(), "GET /regions"); n != 3 {
		t.Fatalf("calls = %d after the TTL, want 3", n)
	}
}

// Real player IP needs the node's kit online and announcing client-addr; a
// node that has no kit yet is reported as such, so a move can mint it.
func TestRemoteNodeKitCarriesClientAddr(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	ctx := context.Background()

	if has, ok, err := g.NodeKitCarriesClientAddr(ctx, 9); err != nil || has || ok {
		t.Fatalf("no kit: %v %v %v", has, ok, err)
	}
	k := mintKit(t, g, 7)
	if has, ok, err := g.NodeKitCarriesClientAddr(ctx, 7); err != nil || !has || ok {
		t.Fatalf("offline kit: %v %v %v", has, ok, err)
	}
	fake.SetKitOnline(k.LinkID, "voice")
	g.dropKitCache()
	if has, ok, _ := g.NodeKitCarriesClientAddr(ctx, 7); !has || ok {
		t.Fatalf("online without client-addr: %v %v", has, ok)
	}
	fake.SetKitOnline(k.LinkID, "client-addr")
	g.dropKitCache()
	if has, ok, err := g.NodeKitCarriesClientAddr(ctx, 7); err != nil || !has || !ok {
		t.Fatalf("online with client-addr: %v %v %v", has, ok, err)
	}
	g.dropKitCache()
	fake.FailNext(1, 500, "http_500", 0)
	if _, ok, err := g.NodeKitCarriesClientAddr(ctx, 7); err == nil || ok {
		t.Fatalf("provider error: ok %v err %v, want a failure", ok, err)
	}
	g.dropKitCache() // the failure is cached for 10 s
	// A kit row the provider no longer lists is not a carrier.
	st.kits[9] = store.RemoteKit{NodeID: 9, LinkID: "link-gone"}
	if has, ok, err := g.NodeKitCarriesClientAddr(ctx, 9); err != nil || !has || ok {
		t.Fatalf("unlisted kit: %v %v %v", has, ok, err)
	}
}

func TestRemoteClientAddrGap(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	ctx := context.Background()
	if n, gap, err := g.ClientAddrGap(ctx, uuidA); err != nil || n != 0 || gap != "" {
		t.Fatalf("no routes: %d %q %v", n, gap, err)
	}
	k7 := mintKit(t, g, 7)
	k9 := mintKit(t, g, 9)
	st.routes["a1.example.com"] = store.RemoteRoute{Domain: "a1.example.com", ServerUUID: uuidA, NodeID: 7}
	st.routes["a2.example.com"] = store.RemoteRoute{Domain: "a2.example.com", ServerUUID: uuidA, NodeID: 9}
	fake.SetKitOnline(k7.LinkID, "client-addr")
	fake.SetKitOnline(k9.LinkID)
	g.dropKitCache()
	if n, gap, err := g.ClientAddrGap(ctx, uuidA); err != nil || n != 2 || gap != "a2.example.com" {
		t.Fatalf("one route behind a kit without the cap: %d %q %v", n, gap, err)
	}
	fake.SetKitOnline(k9.LinkID, "client-addr")
	g.dropKitCache()
	if n, gap, err := g.ClientAddrGap(ctx, uuidA); err != nil || n != 2 || gap != "" {
		t.Fatalf("both carry it: %d %q %v", n, gap, err)
	}
}

func TestRemoteNodeListShowsKitOnline(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	k7 := mintKit(t, g, 7)
	mintKit(t, g, 9)
	fake.SetKitOnline(k7.LinkID, "client-addr")
	g.dropKitCache()

	nodes := []models.Node{{ID: 7}, {ID: 9}, {ID: 11}}
	g.SetNodesLinkOnline(context.Background(), nodes)
	if nodes[0].LinkOnline == nil || !*nodes[0].LinkOnline {
		t.Errorf("node 7 (online kit) = %v, want true", nodes[0].LinkOnline)
	}
	if nodes[1].LinkOnline == nil || *nodes[1].LinkOnline {
		t.Errorf("node 9 (offline kit) = %v, want false", nodes[1].LinkOnline)
	}
	if nodes[2].LinkOnline != nil {
		t.Errorf("node 11 (no kit) = %v, want nil (no answer)", *nodes[2].LinkOnline)
	}
	// One provider call for the whole list.
	if n := countRemoteCalls(fake.Calls(), "GET /link-kits"); n != 1 {
		t.Fatalf("kit list calls = %d, want 1", n)
	}
}

func TestRemoteNodeListSurvivesProviderError(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	mintKit(t, g, 7)
	fake.FailNext(1, 502, "http_502", 0)
	nodes := []models.Node{{ID: 7}}
	g.SetNodesLinkOnline(context.Background(), nodes)
	if nodes[0].LinkOnline != nil {
		t.Fatalf("LinkOnline = %v after a provider error, want nil (no answer)", *nodes[0].LinkOnline)
	}
}

func TestRemoteNodeDeleteRevokesKit(t *testing.T) {
	t.Run("provider revokes", func(t *testing.T) {
		g, st, fake := newRemoteTest(t)
		k := mintKit(t, g, 7)
		mintKit(t, g, 9)
		seedRemoteRoutes(st)
		st.routes["n9.example.com"] = store.RemoteRoute{Domain: "n9.example.com", ServerUUID: uuidB, NodeID: 9}

		g.ForgetNode(context.Background(), 7)
		if countRemoteCalls(fake.Calls(), "DELETE /link-kits/"+k.LinkID) != 1 {
			t.Fatalf("calls %v, want the kit revoked", fake.Calls())
		}
		if _, ok := st.kits[7]; ok {
			t.Fatal("kit row of the deleted node kept")
		}
		if _, ok := st.kits[9]; !ok {
			t.Fatal("another node's kit row removed")
		}
		if len(st.routes) != 1 || st.routes["n9.example.com"].NodeID != 9 {
			t.Fatalf("route rows = %v, want only node 9's", st.routes)
		}
	})
	t.Run("provider fails", func(t *testing.T) {
		g, st, fake := newRemoteTest(t)
		mintKit(t, g, 7)
		seedRemoteRoutes(st)
		fake.FailNext(1, 500, "http_500", 0)
		g.ForgetNode(context.Background(), 7)
		if _, ok := st.kits[7]; ok {
			t.Fatal("kit row kept after a failed revoke; the node is gone, nothing could retry it")
		}
		if len(st.routes) != 0 {
			t.Fatalf("route rows = %v, want none", st.routes)
		}
	})
}

// On the managed gateway a deleted server's routes are the provider's: one
// delete by server, no scan of a Redis that holds none, and the server's own
// housekeeping keys still go.
func TestRemoteServerDeleteSkipsRedis(t *testing.T) {
	rdb := newQueueTestRedis(t)
	seedRoute(t, rdb, GatewayRoute{Domain: "stale.example.com", ServerUUID: "uuid-a"})
	if err := rdb.Set(context.Background(), "dylaris:server:uuid-a:logs", "x", 0).Err(); err != nil {
		t.Fatal(err)
	}
	gw := &routeDeleteGateway{}
	got := RemoveDeletedServers(context.Background(), gw, rdb, []string{"uuid-a"}, remoteOn)
	if got != 0 || len(gw.deleted) != 0 {
		t.Fatalf("matched %d, per-domain deletes %v; want none", got, gw.deleted)
	}
	if len(gw.serverDeletes) != 1 || gw.serverDeletes[0] != "uuid-a" {
		t.Fatalf("server deletes = %v, want [uuid-a]", gw.serverDeletes)
	}
	if !routeExists(t, rdb, "stale.example.com") {
		t.Fatal("the local route key was touched in remote mode")
	}
	if n, _ := rdb.Exists(context.Background(), "dylaris:server:uuid-a:logs").Result(); n != 0 {
		t.Fatal("the server's own keys were left behind")
	}
}

// remoteRouteGateway refuses core-owned routes the way RemoteGateway does.
type remoteRouteGateway struct{ linkRevokeFakeGateway }

func (g *remoteRouteGateway) DeleteCoreOwnedRoute(domain string) error {
	g.deletedDomains = append(g.deletedDomains, domain)
	return ErrGatewayRemote
}

func (f *teardownFakeStore) DeleteCoreLinkRoute(domain string) error {
	f.deletedRows = append(f.deletedRows, domain)
	return nil
}

func (f *linkRevokeFakeStore) DeleteCoreLinkRoute(domain string) error {
	f.deletedRows = append(f.deletedRows, domain)
	return nil
}

// A user who still has a route-only row from before the switch to the managed
// gateway can be deleted: the provider never held that route, so the row is
// all there is to remove.
func TestRemoteUserDeleteWithLeftoverLinkRoute(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &teardownFakeStore{
		keys: []store.WarpAPIKey{{NodeID: "link-1", OwnerID: "owner-1"}},
		rows: []store.CoreLinkRoute{
			{Domain: "kit.example.com", OwnerID: "owner-1", LinkToken: ""},
			{Domain: "other.example.com", OwnerID: "owner-2", LinkToken: ""},
		},
	}
	gw := &remoteRouteGateway{}
	if err := TeardownTenantInfrastructure(ctx, fs, gw, rdb, redisacl.NewProvisioner(rdb), &peerRecorder{}, "owner-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, d := range fs.deletedRows {
		if d != "kit.example.com" {
			t.Fatalf("removed row %s of another owner", d)
		}
	}
	if len(fs.deletedRows) == 0 {
		t.Fatal("the leftover row was not removed")
	}

	// The kit revoke on its own path: same answer, no error.
	ls := &linkRevokeFakeStore{rows: []store.CoreLinkRoute{{Domain: "kit.example.com", OwnerID: "owner-1"}}}
	n, err := RevokeLinkKitTeardown(ctx, ls, &remoteRouteGateway{}, rdb, "link-1", "owner-1")
	if err != nil || n != 1 || len(ls.deletedRows) != 1 {
		t.Fatalf("revoke = %d, %v, rows %v", n, err, ls.deletedRows)
	}
}

// A tenant's nodes go with the account; on the managed gateway their kits are
// revoked and their rows dropped, as on a node delete.
func TestRemoteTenantTeardownRevokesNodeKits(t *testing.T) {
	g, rst, fake := newRemoteTest(t)
	k := mintKit(t, g, 7)
	rst.routes["n7.example.com"] = store.RemoteRoute{Domain: "n7.example.com", ServerUUID: uuidA, NodeID: 7}
	tst := &teardownFakeStore{nodes: []models.Node{{ID: 7, Name: "node-seven", Token: "tok-7"}}}
	gw := NewSwitchingGateway(nil, g, remoteOn)

	rdb := newQueueTestRedis(t)
	if err := TeardownTenantInfrastructure(context.Background(), tst, gw, rdb, redisacl.NewProvisioner(rdb), &peerRecorder{}, "owner-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if countRemoteCalls(fake.Calls(), "DELETE /link-kits/"+k.LinkID) != 1 {
		t.Fatalf("calls %v, want the kit revoked", fake.Calls())
	}
	if len(rst.kits) != 0 || len(rst.routes) != 0 || len(tst.deletedNodes) != 1 {
		t.Fatalf("kits %v routes %v deleted nodes %v", rst.kits, rst.routes, tst.deletedNodes)
	}

	// While the mode is unknown nothing is removed: the kit would be lost.
	g2, rst2, _ := newRemoteTest(t)
	mintKit(t, g2, 7)
	tst2 := &teardownFakeStore{nodes: []models.Node{{ID: 7, Name: "node-seven", Token: "tok-7"}}}
	unknown := func() (bool, bool) { return false, false }
	err := TeardownTenantInfrastructure(context.Background(), tst2, NewSwitchingGateway(nil, g2, unknown),
		rdb, redisacl.NewProvisioner(rdb), &peerRecorder{}, "owner-1")
	if !errors.Is(err, ErrGatewayProviderUnknown) || len(tst2.deletedNodes) != 0 || len(rst2.kits) != 1 {
		t.Fatalf("unknown mode: err %v, deleted %v, kits %v", err, tst2.deletedNodes, rst2.kits)
	}
}
