package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

// ownRowsOnProvider puts own-domain rows of server 1 (tenant A, node 9) with
// A's verified local claim, each with a provider claim and a provider route.
func ownRowsOnProvider(t *testing.T, g *RemoteGateway, st *remoteFakeStore, h *lapseHandle, domains ...string) {
	t.Helper()
	st.servers[1].NodeID = 9
	for _, d := range domains {
		ownRow(st, d, uuidA, ownerA)
		if _, err := g.EnsureProviderClaim(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		h.fake.AddRoute(st.kits[9].LinkID, d, "mc-"+uuidA, 25565)
	}
}

// Two replicas: this one (the sweeping leader) cached the claims as pending a
// moment ago; another replica has since verified them and created the routes.
// The negative verdict is re-read uncached, once for the whole pass, and the
// proven routes stay.
func TestReconcileStaleClaimCacheKeepsAProvenRoute(t *testing.T) {
	g, st, h := newLapseTest(t)
	ctx := context.Background()
	domains := []string{"play.own.net", "mc.own.net"}
	ownRowsOnProvider(t, g, st, h, domains...)
	if _, err := g.ProviderCustomDomains(ctx); err != nil { // cached: pending
		t.Fatal(err)
	}
	for _, d := range domains {
		h.fake.SetCustomDomainState(d, store.ClaimVerified)
	}
	before := countRemoteCalls(h.fake.Calls(), "GET /custom-domains")
	reconcileWith(t, st, h.c, g)
	if len(st.outbox) != 0 {
		t.Fatalf("outbox = %+v, want nothing: the routes are proven", st.outbox)
	}
	if n := countRemoteCalls(h.fake.Calls(), "GET /custom-domains") - before; n != 1 {
		t.Fatalf("claims list read %d times in the pass, want exactly 1 (the fresh read)", n)
	}
}

// A claim that is unverified in the fresh read too is deleted, and the fresh
// list serves every row of the pass.
func TestReconcileFreshClaimReadStillDeletesAnUnverifiedRoute(t *testing.T) {
	g, st, h := newLapseTest(t)
	domains := []string{"play.own.net", "mc.own.net"}
	ownRowsOnProvider(t, g, st, h, domains...)
	if _, err := g.ProviderCustomDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := countRemoteCalls(h.fake.Calls(), "GET /custom-domains")
	reconcileWith(t, st, h.c, g)
	for _, d := range domains {
		if ops := openOps(st, d); len(ops) != 1 || ops[0] != OutboxOpDelete {
			t.Fatalf("%s: open ops = %v, want the delete", d, ops)
		}
	}
	if n := countRemoteCalls(h.fake.Calls(), "GET /custom-domains") - before; n != 1 {
		t.Fatalf("claims list read %d times in the pass, want 1", n)
	}
}

// The provider lists a route on the domain but no claim: it refuses to release
// a claim a live route uses, so that list is not this platform's truth and the
// row is left alone (and reported). Without a provider route the missing claim
// is real and the route goes.
func TestReconcileRouteWithoutProviderClaimIsSkipped(t *testing.T) {
	g, st, h := newLapseTest(t)
	st.servers[1].NodeID = 9
	ownRow(st, "listed.own.net", uuidA, ownerA)
	h.fake.AddRoute(st.kits[9].LinkID, "listed.own.net", "mc-"+uuidA, 25565)
	ownRow(st, "gone.own.net", uuidA, ownerA)
	logs := captureLog(t)
	reconcileWith(t, st, h.c, g)
	if ops := openOps(st, "listed.own.net"); len(ops) != 0 {
		t.Fatalf("listed.own.net: open ops = %v, want none", ops)
	}
	if !strings.Contains(logs.String(), "address listed.own.net left alone this pass") {
		t.Fatalf("not reported: %s", logs.String())
	}
	if ops := openOps(st, "gone.own.net"); len(ops) != 1 || ops[0] != OutboxOpDelete {
		t.Fatalf("gone.own.net: open ops = %v, want the delete", ops)
	}
}

// The fresh read fails: nothing is queued on the stale "pending".
func TestReconcileFailedFreshClaimReadQueuesNothing(t *testing.T) {
	g, st, h := newLapseTest(t)
	st.servers[1].NodeID = 9
	ownRow(st, "play.own.net", uuidA, ownerA) // not on the provider: a missing claim would read as real
	if _, err := g.EnsureProviderClaim(context.Background(), "play.own.net"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ProviderCustomDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The route list answers, the claims list after it does not.
	h.fake.FailAfter(1, 1, 503, "unavailable", 0)
	reconcileWith(t, st, h.c, g)
	if len(st.outbox) != 0 {
		t.Fatalf("outbox = %+v, want nothing", st.outbox)
	}
}

// A provider claim no user here holds is released by the sweep; one with a
// pending local claim, a route row, or a release already queued is not.
func TestSweepReleasesOrphanedProviderClaims(t *testing.T) {
	g, st, h := newLapseTest(t)
	ctx := context.Background()
	for _, d := range []string{"orphan.own.net", "pending.own.net", "routed.own.net", "queued.own.net"} {
		if _, err := g.EnsureProviderClaim(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	st.claims = append(st.claims, store.CustomDomainClaim{UserID: ownerA, Domain: "pending.own.net", State: store.ClaimPending})
	// The operator's own route (no owner, so no local claim): it uses the
	// provider claim, which the sweep must keep.
	st.servers[1].OwnerID, st.servers[1].NodeID = "", 9
	ownRow(st, "routed.own.net", uuidA, "")
	h.fake.SetCustomDomainState("routed.own.net", store.ClaimVerified)
	h.fake.AddRoute(st.kits[9].LinkID, "routed.own.net", "mc-"+uuidA, 25565)
	if err := st.EnqueueOutbox(OutboxOpDeleteCustomDomain, "queued.own.net", struct{}{}); err != nil {
		t.Fatal(err)
	}
	st.outbox[len(st.outbox)-1].next = time.Now().Add(time.Hour) // waiting, so the tick's drain leaves it
	l := &outboxLoop{st: st, api: func() *remotegw.Client { return h.c }, mode: func() (bool, bool) { return true, true }, now: time.Now, rg: g}
	l.tick(ctx)
	if ops := openOps(st, "orphan.own.net"); len(ops) != 1 || ops[0] != OutboxOpDeleteCustomDomain {
		t.Fatalf("orphan: open ops = %v, want its release", ops)
	}
	for _, d := range []string{"pending.own.net", "routed.own.net"} {
		if ops := openOps(st, d); len(ops) != 0 {
			t.Fatalf("%s: open ops = %v, want none", d, ops)
		}
	}
	if ops := openOps(st, "queued.own.net"); len(ops) != 1 {
		t.Fatalf("queued: open ops = %v, want the one release", ops)
	}
}

// A local claim count that fails releases nothing.
func TestSweepReleasesNothingWithoutTheLocalCount(t *testing.T) {
	g, st, _ := newLapseTest(t)
	if _, err := g.EnsureProviderClaim(context.Background(), "orphan.own.net"); err != nil {
		t.Fatal(err)
	}
	st.countErr = errors.New("db down")
	if n, err := releaseOrphanProviderClaims(context.Background(), st, g); err == nil || n != 0 {
		t.Fatalf("n %d err %v, want the count error", n, err)
	}
	if len(st.outbox) != 0 {
		t.Fatalf("outbox = %+v", st.outbox)
	}
}

// At most ownDomainWritesPerMin own-domain claim writes a minute leave this
// Core: the next is refused without a provider call, a queued release waits
// for the window without an attempt counted, and route writes are not held.
func TestOwnDomainClaimWritesCeiling(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	c := remotegw.New(fake.URL(), fake.Key)
	now := outboxT0
	g.now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < ownDomainWritesPerMin; i++ {
		if _, err := g.VerifyProviderClaim(ctx, "x.own.net"); errors.Is(err, ErrOwnDomainWritesBusy) {
			t.Fatalf("write %d refused", i)
		}
	}
	sent := len(fake.Calls())
	if _, err := g.VerifyProviderClaim(ctx, "x.own.net"); !errors.Is(err, ErrOwnDomainWritesBusy) {
		t.Fatalf("over the ceiling: %v", err)
	}
	if _, err := g.EnsureProviderClaim(ctx, "y.own.net"); !errors.Is(err, ErrOwnDomainWritesBusy) {
		t.Fatalf("add over the ceiling: %v", err)
	}
	if calls := fake.Calls()[sent:]; countRemoteCalls(calls, "POST /custom-domains") != 0 {
		t.Fatalf("refused writes reached the provider: %v", calls)
	}
	if err := st.EnqueueOutbox(OutboxOpDeleteCustomDomain, "gone.own.net", struct{}{}); err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return now }
	if done, failed, _ := drainOutboxOnce(ctx, st, c, nil, clock, g); done != 0 || failed != 0 {
		t.Fatalf("drain done %d failed %d, want the release deferred", done, failed)
	}
	it := st.outbox[0]
	if it.attempts != 0 || !it.next.Equal(outboxT0.Add(time.Minute)) || countRemoteCalls(fake.Calls(), "DELETE /custom-domains") != 0 {
		t.Fatalf("item %+v, calls %v", it, fake.Calls())
	}
	if err := g.CreateServerRoute(1, "", "r.example.com", 25565); err != nil {
		t.Fatalf("a route write was held by the own-domain ceiling: %v", err)
	}
	now = now.Add(time.Minute)
	if done, _, _ := drainOutboxOnce(ctx, st, c, nil, clock, g); done != 1 {
		t.Fatalf("drain after the window: done %d", done)
	}
}

// A hoster-domain row whose repair the provider refuses for the domain stays,
// and the refusal is reported once, not on every sweep.
func TestReconcileHosterRowRefusedForTheDomainStays(t *testing.T) {
	st, fake, c, _ := newOutboxTest(t)
	const d = "refused-hoster.example.com"
	st.routes[d] = store.RemoteRoute{Domain: d, ServerUUID: uuidA, NodeID: 9, Port: 25565}
	logs := captureLog(t)
	for i := 0; i < 2; i++ {
		fake.FailAfter(1, 1, 403, "domain_unverified", 0)
		reconcileOK(t, st, c)
	}
	if _, ok := st.routes[d]; !ok || len(st.outbox) != 0 {
		t.Fatalf("row kept %v, outbox %+v", ok, st.outbox)
	}
	if n := strings.Count(logs.String(), "route "+d+" not repaired"); n != 1 {
		t.Fatalf("reported %d times, want once: %s", n, logs.String())
	}
}

// A caller that has decided the domain is an own one passes that decision on:
// the row carries it and the hoster domains are not read again.
func TestRemoteCreateTakesTheCallersOwnDomainDecision(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	verifyOnProvider(t, g, fake, "play.own.net")
	if err := g.CreateServerRouteOwnDomain(1, "play.own.net", 25565, true); err != nil {
		t.Fatal(err)
	}
	if r := st.routes["play.own.net"]; !r.CustomDomain {
		t.Fatalf("row = %+v", r)
	}
	if n := countRemoteCalls(fake.Calls(), "GET /regions"); n != 0 {
		t.Fatalf("hoster domains read %d times", n)
	}
}
