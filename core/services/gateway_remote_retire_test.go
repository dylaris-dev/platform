package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

func (f *remoteFakeStore) SetRemoteKitRetireAt(nodeID uint, linkID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stampErr != nil {
		return f.stampErr
	}
	if k, ok := f.kits[nodeID]; ok && k.LinkID == linkID {
		k.RetireAt = &at
		f.kits[nodeID] = k
	}
	return nil
}

// A node's kit replaced at a host limit of one: the other node can get a kit
// at once, and the retiring node keeps its kit rather than getting a second.
func TestRemoteRetireNodeKitFreesAnotherNode(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(1)
	old, err := g.EnsureKit(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.EnsureKit(ctx, 9); !remotegw.IsCode(err, "link_limit") {
		t.Fatalf("second node at the limit: %v, want link_limit", err)
	}
	before := time.Now()
	at, err := g.RetireNodeKit(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if d := at.Sub(before); d < 8*time.Hour-2*time.Second || d > 8*time.Hour+2*time.Second {
		t.Fatalf("retire_at %v away, want 8h", d)
	}
	if k := st.kits[7]; k.RetireAt == nil || !k.RetireAt.Equal(at) || k.LinkID != old.LinkID {
		t.Fatalf("local row %+v, want stamped with %v", k, at)
	}
	if _, err := g.EnsureKit(ctx, 9); err != nil {
		t.Fatalf("other node after the retire: %v", err)
	}
	// Retiring again answers the same deadline without asking the provider.
	n := countRemoteCalls(fake.Calls(), "POST /link-kits/")
	if again, err := g.RetireNodeKit(ctx, 7); err != nil || !again.Equal(at) || countRemoteCalls(fake.Calls(), "POST /link-kits/") != n {
		t.Fatalf("second retire = %v, %v; calls %v", again, err, fake.Calls())
	}
	if _, err := g.RetireNodeKit(ctx, 42); !errors.Is(err, ErrGatewayNoKit) {
		t.Fatalf("node without a kit: %v", err)
	}
	// The provider's refusals reach the caller with their codes.
	if _, err := g.RetireNodeKit(ctx, 9); !remotegw.IsCode(err, "retire_limit") {
		t.Fatalf("second retire at one host: %v, want retire_limit", err)
	}
}

// A retire that landed on the provider but not here is stamped with the
// provider's deadline on the next try.
func TestRemoteRetireNodeKitTakesTheProvidersDeadline(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(1)
	k, err := g.EnsureKit(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	at, err := remotegw.New(fake.URL(), fake.Key).RetireKit(ctx, k.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.RetireNodeKit(ctx, 7)
	if err != nil || !got.Equal(at) || st.kits[7].RetireAt == nil || !st.kits[7].RetireAt.Equal(at) {
		t.Fatalf("got %v, %v, row %+v; want %v", got, err, st.kits[7], at)
	}
}

// While a node's kit is retiring EnsureKit returns it (the deploy kit still
// shows it) and mints nothing; once the provider removed it and reconcile
// forgot it, the node gets a fresh kit.
func TestRemoteEnsureKitReturnsRetiringKit(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(1)
	old, err := g.EnsureKit(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	at, err := g.RetireNodeKit(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	k, err := g.EnsureKit(ctx, 7)
	if err != nil || k.LinkID != old.LinkID || k.RetireAt == nil || !k.RetireAt.Equal(at) {
		t.Fatalf("EnsureKit while retiring = %+v, %v", k, err)
	}
	if n := countRemoteCalls(fake.Calls(), "POST /link-kits"); n != 2 { // mint + retire
		t.Fatalf("calls %v, want one mint and one retire", fake.Calls())
	}
	// The provider's deadline tick, then the sweep after it.
	cl := remotegw.New(fake.URL(), fake.Key)
	if err := cl.RevokeKit(ctx, old.LinkID); err != nil {
		t.Fatal(err)
	}
	if n, err := reconcileRemoteKits(ctx, st, cl, at.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v", n, err)
	}
	fresh, err := g.EnsureKit(ctx, 7)
	if err != nil || fresh.LinkID == old.LinkID || fresh.RetireAt != nil {
		t.Fatalf("after the kit is gone: %+v, %v", fresh, err)
	}
}

// No new route and no move onto a node whose kit is retiring: refused here,
// before the provider is asked or a row claimed.
func TestRemoteCreateRouteRefusedOnRetiringKit(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(2)
	if err := g.CreateServerRoute(1, "owner", "a.example.com", 25565); err != nil {
		t.Fatal(err)
	}
	if _, err := g.RetireNodeKit(ctx, 7); err != nil {
		t.Fatal(err)
	}
	puts := countRemoteCalls(fake.Calls(), "POST /routes")
	err := g.CreateServerRoute(2, "owner", "b.example.com", 25565)
	if !errors.Is(err, ErrGatewayKitRetiring) || err.Error() != "this node's link is being replaced; move the server to another node first" {
		t.Fatalf("create on a retiring node: %v", err)
	}
	if _, held := st.routes["b.example.com"]; held || countRemoteCalls(fake.Calls(), "POST /routes") != puts {
		t.Fatalf("refused create claimed %v or reached the provider %v", st.routes, fake.Calls())
	}
	// The same server's own address again is a route change too.
	if err := g.CreateServerRoute(1, "owner", "a.example.com", 25565); !errors.Is(err, ErrGatewayKitRetiring) {
		t.Fatalf("re-create on a retiring node: %v", err)
	}
	if _, held := st.routes["a.example.com"]; !held {
		t.Fatal("the refused re-create released the server's existing address")
	}

	// A move onto the retiring node queues nothing; moving away is fine.
	st.servers[1].NodeID = 9
	st.routes["a.example.com"] = store.RemoteRoute{Domain: "a.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	if err := g.MigrateServerRoutes(1, 7); !errors.Is(err, ErrGatewayKitRetiring) {
		t.Fatalf("move onto the retiring node: %v", err)
	}
	if len(st.outbox) != 0 {
		t.Fatalf("refused move queued %+v", st.outbox)
	}
	if err := g.MigrateServerRoutes(1, 9); err != nil || len(st.outbox) != 1 || st.outbox[0].op != OutboxOpMove {
		t.Fatalf("move away: %v, outbox %+v", err, st.outbox)
	}
}

// The cleanup view lists only provider kits no local row names, flags the
// retiring ones, and is empty when everything is known.
func TestRemoteOrphanKitsExcludesKnownKits(t *testing.T) {
	ctx := context.Background()
	g, _, fake := newRemoteTest(t)
	if _, err := g.EnsureKit(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if orphans, err := g.OrphanKits(ctx); err != nil || orphans == nil || len(orphans) != 0 {
		t.Fatalf("only known kits: %v, %v; want an empty list", orphans, err)
	}
	cl := remotegw.New(fake.URL(), fake.Key)
	lost, err := cl.MintKit(ctx, "lost", "eu")
	if err != nil {
		t.Fatal(err)
	}
	g.dropKitCache()
	orphans, err := g.OrphanKits(ctx)
	if err != nil || len(orphans) != 1 || orphans[0].LinkID != lost.LinkID || orphans[0].Name != "lost" || orphans[0].RetireAt != nil {
		t.Fatalf("orphans = %+v, %v; want only %s", orphans, err, lost.LinkID)
	}
}

func TestRemoteOrphanKitsFlagsRetiring(t *testing.T) {
	ctx := context.Background()
	g, _, fake := newRemoteTest(t)
	fake.SetLinkLimit(3)
	cl := remotegw.New(fake.URL(), fake.Key)
	a, _ := cl.MintKit(ctx, "a", "eu")
	b, _ := cl.MintKit(ctx, "b", "eu")
	at, err := cl.RetireKit(ctx, b.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	orphans, err := g.OrphanKits(ctx)
	if err != nil || len(orphans) != 2 {
		t.Fatalf("orphans = %+v, %v", orphans, err)
	}
	for _, o := range orphans {
		switch o.LinkID {
		case a.LinkID:
			if o.RetireAt != nil {
				t.Fatalf("%s flagged retiring: %+v", a.LinkID, o)
			}
		case b.LinkID:
			if o.RetireAt == nil || !o.RetireAt.Equal(at) {
				t.Fatalf("%s not flagged retiring until %v: %+v", b.LinkID, at, o)
			}
		default:
			t.Fatalf("unexpected orphan %+v", o)
		}
	}
}

// Review focus 4: a kit a local row names is never revoked from the cleanup
// view, also when the cached list still shows it as unknown (another replica
// stored it a moment ago).
func TestRemoteRevokeOrphanRefusesKnownKit(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	cl := remotegw.New(fake.URL(), fake.Key)
	m, err := cl.MintKit(ctx, "node-nine", "")
	if err != nil {
		t.Fatal(err)
	}
	if orphans, _ := g.OrphanKits(ctx); len(orphans) != 1 {
		t.Fatalf("orphans = %+v", orphans)
	}
	st.kits[9] = store.RemoteKit{NodeID: 9, LinkID: m.LinkID}
	if queued, err := g.RevokeOrphanKit(ctx, m.LinkID); !errors.Is(err, ErrGatewayKitKnown) || queued {
		t.Fatalf("revoke of a known kit = %v, %v", queued, err)
	}
	if n := countRemoteCalls(fake.Calls(), "DELETE "); n != 0 || len(st.outbox) != 0 {
		t.Fatalf("refused revoke reached the provider %v or queued %+v", fake.Calls(), st.outbox)
	}
	if kits, _ := cl.Kits(ctx); len(kits) != 1 {
		t.Fatalf("provider kits = %+v, want the kit kept", kits)
	}
}

func TestRemoteRevokeOrphanQueuesOnRetryableFailure(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	cl := remotegw.New(fake.URL(), fake.Key)
	m, _ := cl.MintKit(ctx, "lost", "")

	fake.FailNext(1, 403, "forbidden", 0)
	if queued, err := g.RevokeOrphanKit(ctx, m.LinkID); !remotegw.IsCode(err, "forbidden") || queued || len(st.outbox) != 0 {
		t.Fatalf("refusal: %v, %v, outbox %+v; want the error, nothing queued", queued, err, st.outbox)
	}
	fake.FailNext(1, 503, "unavailable", 0)
	queued, err := g.RevokeOrphanKit(ctx, m.LinkID)
	if err != nil || !queued || len(st.outbox) != 1 || st.outbox[0].op != OutboxOpRevokeKit || st.outbox[0].domain != m.LinkID {
		t.Fatalf("retryable: %v, %v, outbox %+v; want a revoke_kit for %s", queued, err, st.outbox, m.LinkID)
	}
	// The drain replays it; then the kit is gone from the provider.
	if done, _, _ := drainOutboxOnce(ctx, st, cl, nil, time.Now, nil); done != 1 {
		t.Fatalf("drain done = %d, outbox %+v", done, st.outbox)
	}
	if kits, _ := cl.Kits(ctx); len(kits) != 0 {
		t.Fatalf("provider kits after the drain: %+v", kits)
	}
	// Already gone counts as done, and the view stops listing it.
	if queued, err := g.RevokeOrphanKit(ctx, m.LinkID); err != nil || queued {
		t.Fatalf("revoke of a gone kit: %v, %v", queued, err)
	}
	if orphans, err := g.OrphanKits(ctx); err != nil || len(orphans) != 0 {
		t.Fatalf("orphans after the revoke: %+v, %v", orphans, err)
	}
}

func TestReconcileDropsRetiredKitRow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	_, st, fake := newRemoteTest(t)
	cl := remotegw.New(fake.URL(), fake.Key)
	listed, _ := cl.MintKit(ctx, "still-listed", "")
	// Node 7: retired, deadline passed, the provider removed it.
	st.kits[7] = store.RemoteKit{NodeID: 7, LinkID: "link-gone", RetireAt: &past}
	// Node 9: retired, deadline passed, but the provider's tick has not run.
	st.kits[9] = store.RemoteKit{NodeID: 9, LinkID: listed.LinkID, RetireAt: &past}
	// Node 11: retiring, deadline ahead. Node 12: lost on the provider, not retired.
	st.kits[11] = store.RemoteKit{NodeID: 11, LinkID: "link-future", RetireAt: &future}
	st.kits[12] = store.RemoteKit{NodeID: 12, LinkID: "link-lost"}
	st.routes["dead.example.com"] = store.RemoteRoute{Domain: "dead.example.com", ServerUUID: uuidA, NodeID: 7}
	st.routes["moving.example.com"] = store.RemoteRoute{Domain: "moving.example.com", ServerUUID: uuidB, NodeID: 7}
	st.outbox = append(st.outbox, remoteOutboxItem{op: OutboxOpMove, domain: "moving.example.com", id: 1})
	st.routes["other.example.com"] = store.RemoteRoute{Domain: "other.example.com", ServerUUID: uuidA, NodeID: 12}

	n, err := reconcileRemoteKits(ctx, st, cl, now)
	if err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v; want node 7's kit forgotten", n, err)
	}
	if _, ok := st.kits[7]; ok {
		t.Fatal("the removed retired kit's row is still there")
	}
	for _, id := range []uint{9, 11, 12} {
		if _, ok := st.kits[id]; !ok {
			t.Fatalf("kit row of node %d was dropped", id)
		}
	}
	if _, ok := st.routes["dead.example.com"]; ok {
		t.Fatal("a route that died with the kit kept its row")
	}
	for _, d := range []string{"moving.example.com", "other.example.com"} {
		if _, ok := st.routes[d]; !ok {
			t.Fatalf("route row %s was dropped", d)
		}
	}

	// No kit rows at all: the provider is not asked.
	calls := len(fake.Calls())
	st.kits = map[uint]store.RemoteKit{}
	if n, err := reconcileRemoteKits(ctx, st, cl, now); err != nil || n != 0 || len(fake.Calls()) != calls {
		t.Fatalf("no rows: %d, %v, calls %v", n, err, fake.Calls()[calls:])
	}
}

// The provider committed the retire, its answer was lost: the kit list says
// so, and the row is stamped with the provider's deadline.
func TestRemoteRetireLostAnswerIsStamped(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(1)
	k, err := g.EnsureKit(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	fake.LoseNextAnswer()
	at, err := g.RetireNodeKit(ctx, 7)
	if err != nil || at.IsZero() {
		t.Fatalf("retire with a lost answer = %v, %v; want the provider's deadline", at, err)
	}
	kits, _ := remotegw.New(fake.URL(), fake.Key).Kits(ctx)
	if len(kits) != 1 || kits[0].LinkID != k.LinkID || kits[0].RetireAt == nil || !kits[0].RetireAt.Equal(at) {
		t.Fatalf("provider %+v, want %s retiring until %v", kits, k.LinkID, at)
	}
	if r := st.kits[7].RetireAt; r == nil || !r.Equal(at) {
		t.Fatalf("row %+v not stamped", st.kits[7])
	}

	// A lost answer on a retire the provider did NOT commit stays an error.
	g2, st2, fake2 := newRemoteTest(t)
	fake2.SetLinkLimit(1)
	if _, err := g2.EnsureKit(ctx, 7); err != nil {
		t.Fatal(err)
	}
	fake2.FailNext(1, 503, "unavailable", 0)
	if _, err := g2.RetireNodeKit(ctx, 7); err == nil || st2.kits[7].RetireAt != nil {
		t.Fatalf("uncommitted retire: %v, row %+v", err, st2.kits[7])
	}
	// A definite refusal is not looked up.
	before := countRemoteCalls(fake2.Calls(), "GET /link-kits")
	fake2.FailNext(1, 409, "retire_budget", 0)
	if _, err := g2.RetireNodeKit(ctx, 7); !remotegw.IsCode(err, "retire_budget") || countRemoteCalls(fake2.Calls(), "GET /link-kits") != before {
		t.Fatalf("refusal: %v, calls %v", err, fake2.Calls())
	}
}

// The local stamp failed after the provider's commit: the next sweep copies
// the provider's deadline onto the row.
func TestReconcileStampsAMissedRetire(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(1)
	if _, err := g.EnsureKit(ctx, 7); err != nil {
		t.Fatal(err)
	}
	st.stampErr = errors.New("database down")
	if _, err := g.RetireNodeKit(ctx, 7); err == nil {
		t.Fatal("a failed stamp was reported as done")
	}
	st.stampErr = nil
	if st.kits[7].RetireAt != nil {
		t.Fatal("row stamped despite the failure")
	}
	cl := remotegw.New(fake.URL(), fake.Key)
	if n, err := reconcileRemoteKits(ctx, st, cl, time.Now()); err != nil || n != 0 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	kits, _ := cl.Kits(ctx)
	if r := st.kits[7].RetireAt; r == nil || kits[0].RetireAt == nil || !r.Equal(*kits[0].RetireAt) {
		t.Fatalf("row %+v, provider %+v", st.kits[7], kits)
	}
}

// A kit the provider no longer has (revoked at its deadline while the stamp
// was missing, or removed on the provider) is forgotten once it is older than
// ten minutes, with its addresses, so the node can get a fresh one. A kit
// minted a minute ago, or a sweep whose kit list failed, forgets nothing.
func TestReconcileForgetsALostKit(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	g, st, fake := newRemoteTest(t)
	cl := remotegw.New(fake.URL(), fake.Key)
	st.kits[7] = store.RemoteKit{NodeID: 7, LinkID: "link-dead", CreatedAt: now.Add(-11 * time.Minute)}
	st.kits[9] = store.RemoteKit{NodeID: 9, LinkID: "link-fresh", CreatedAt: now.Add(-time.Minute)}
	st.routes["dead.example.com"] = store.RemoteRoute{Domain: "dead.example.com", ServerUUID: uuidA, NodeID: 7}
	st.routes["fresh.example.com"] = store.RemoteRoute{Domain: "fresh.example.com", ServerUUID: uuidB, NodeID: 9}

	fake.FailNext(1, 503, "unavailable", 0)
	if n, err := reconcileRemoteKits(ctx, st, cl, now); err == nil || n != 0 || len(st.kits) != 2 || len(st.routes) != 2 {
		t.Fatalf("failed list: %d, %v, kits %v, routes %v; want nothing dropped", n, err, st.kits, st.routes)
	}
	n, err := reconcileRemoteKits(ctx, st, cl, now)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if _, ok := st.kits[7]; ok {
		t.Fatal("the dead kit's row is still there")
	}
	if _, ok := st.routes["dead.example.com"]; ok {
		t.Fatal("the dead kit's address row is still there")
	}
	if _, ok := st.kits[9]; !ok {
		t.Fatal("a kit minted a minute ago was dropped")
	}
	if _, ok := st.routes["fresh.example.com"]; !ok {
		t.Fatal("the fresh kit's address row was dropped")
	}
	fresh, err := g.EnsureKit(ctx, 7)
	if err != nil || fresh.LinkID == "link-dead" {
		t.Fatalf("fresh kit for the node = %+v, %v", fresh, err)
	}
}

// Remote mode only: a stale retire_at row from an earlier remote period does
// not exclude a node while this platform runs its own gateway.
func TestRetiringKitNodes(t *testing.T) {
	_, st, _ := newRemoteTest(t)
	at := time.Now().Add(time.Hour)
	st.kits[7] = store.RemoteKit{NodeID: 7, LinkID: "link-a", RetireAt: &at}
	st.kits[9] = store.RemoteKit{NodeID: 9, LinkID: "link-b"}
	for _, c := range []struct {
		name         string
		remote, kn   bool
		want7, want9 bool
	}{
		{"remote", true, true, true, false},
		{"local", false, true, false, false},
		{"mode unknown", true, false, false, false},
	} {
		got, err := RetiringKitNodes(st, func() (bool, bool) { return c.remote, c.kn })
		if err != nil || got[7] != c.want7 || got[9] != c.want9 {
			t.Fatalf("%s: %v, %v", c.name, got, err)
		}
	}
}

// The route repair puts nothing onto a link being replaced: the provider
// would refuse it with kit_retiring on every sweep.
func TestReconcileRepairSkipsRetiringKit(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(2)
	if _, err := g.EnsureKit(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := g.RetireNodeKit(ctx, 7); err != nil {
		t.Fatal(err)
	}
	st.routes["gone.example.com"] = store.RemoteRoute{Domain: "gone.example.com", ServerUUID: uuidA, NodeID: 7, Port: 25565}
	before := countRemoteCalls(fake.Calls(), "POST /routes")
	if _, repaired, _, err := reconcileRemoteRoutes(ctx, st, remotegw.New(fake.URL(), fake.Key), nil); err != nil || repaired != 0 {
		t.Fatalf("reconcile: repaired %d, %v", repaired, err)
	}
	if countRemoteCalls(fake.Calls(), "POST /routes") != before {
		t.Fatalf("a route was put onto the retiring kit: %v", fake.Calls())
	}
}

// A caller that went away (closed tab) does not cut the retire off between
// the provider's commit and the local stamp.
func TestRemoteRetireOutlivesTheCaller(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	fake.SetLinkLimit(1)
	if _, err := g.EnsureKit(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if at, err := g.RetireNodeKit(ctx, 7); err != nil || st.kits[7].RetireAt == nil || !st.kits[7].RetireAt.Equal(at) {
		t.Fatalf("retire on a cancelled context = %v, %v, row %+v", at, err, st.kits[7])
	}
}
