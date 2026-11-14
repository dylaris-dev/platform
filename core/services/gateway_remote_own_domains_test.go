package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/store"
)

func TestOwnDomain(t *testing.T) {
	hosters := []remotegw.HosterDomain{{Domain: "play.e2e.test"}, {Domain: "Example.COM."}}
	for _, c := range []struct {
		domain string
		own    bool
	}{
		{"play.e2e.test", false},
		{"PLAY.e2e.test.", false},
		{"mc.play.e2e.test", false},
		{"a.b.play.e2e.test", false},
		{"evilplay.e2e.test", true},
		{"e2e.test", true},
		{"example.com", false},
		{"x.example.com", false},
		{"play.own.net", true},
		{"example.com.evil.net", true},
	} {
		if got := OwnDomain(c.domain, hosters); got != c.own {
			t.Errorf("OwnDomain(%q) = %v, want %v", c.domain, got, c.own)
		}
	}
	if !OwnDomain("x.example.com", nil) {
		t.Error("with no hoster domains every domain is an own one")
	}
}

// The row records whether the domain is an own one; a hoster-domain route
// (the fake's hoster domain is example.com) stays false.
func TestRemoteCreateMarksOwnDomainRoutes(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	verifyOnProvider(t, g, fake, "play.own.net")
	if err := g.CreateServerRoute(1, "owner", "a.example.com", 25565); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateServerRoute(1, "owner", "play.own.net", 25565); err != nil {
		t.Fatal(err)
	}
	if st.routes["a.example.com"].CustomDomain || !st.routes["play.own.net"].CustomDomain {
		t.Fatalf("rows = %+v", st.routes)
	}
	// The same server creating it again keeps the flag on the upserted row.
	if err := g.CreateServerRoute(1, "owner", "play.own.net", 25570); err != nil {
		t.Fatal(err)
	}
	if r := st.routes["play.own.net"]; !r.CustomDomain || r.Port != 25570 {
		t.Fatalf("re-create row = %+v", r)
	}
}

// Not knowing the hoster domains refuses the create before anything is
// claimed: a row marked wrongly would escape the claim checks.
func TestRemoteCreateRefusesWithoutHosterDomains(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	fake.FailNext(1, 503, "unavailable", 0)
	if err := g.CreateServerRoute(1, "owner", "play.own.net", 25565); !remotegw.IsCode(err, "unavailable") {
		t.Fatalf("err = %v", err)
	}
	if len(st.routes) != 0 || countRemoteCalls(fake.Calls(), "POST") != 0 {
		t.Fatalf("rows %v, calls %v", st.routes, fake.Calls())
	}
}

func TestRemoteEnsureProviderClaimIsIdempotent(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	ctx := context.Background()
	a, err := g.EnsureProviderClaim(ctx, "play.own.net")
	if err != nil || a.TXTValue == "" || a.State != "pending" {
		t.Fatalf("first = %+v, %v", a, err)
	}
	b, err := g.EnsureProviderClaim(ctx, "play.own.net")
	if err != nil || b.TXTValue != a.TXTValue {
		t.Fatalf("second = %+v, %v; want TXT value %q", b, err, a.TXTValue)
	}
	claims, err := g.ProviderCustomDomains(ctx)
	if err != nil || len(claims) != 1 || claims["play.own.net"].TXTValue != a.TXTValue {
		t.Fatalf("provider claims = %+v, %v", claims, err)
	}
	// The second call finds the claim with its value in the cached list: no
	// provider write.
	if n := countRemoteCalls(fake.Calls(), "POST /custom-domains"); n != 1 {
		t.Fatalf("adds = %d (%v), want 1", n, fake.Calls())
	}
	// A failed list read is not "no claim": the write still happens.
	g, _, fake = newRemoteTest(t)
	fake.FailNext(1, 503, "unavailable", 0)
	if c, err := g.EnsureProviderClaim(ctx, "play.own.net"); err != nil || c.TXTValue == "" {
		t.Fatalf("list down: %+v, %v", c, err)
	}
	if n := countRemoteCalls(fake.Calls(), "POST /custom-domains"); n != 1 {
		t.Fatalf("list down: adds = %d (%v)", n, fake.Calls())
	}
}

// A claim write drops the cached list; between writes the list is read once.
func TestRemoteProviderCustomDomainsCache(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := g.ProviderCustomDomains(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRemoteCalls(fake.Calls(), "GET /custom-domains"); n != 1 {
		t.Fatalf("list reads = %d, want 1", n)
	}
	if _, err := g.EnsureProviderClaim(ctx, "play.own.net"); err != nil {
		t.Fatal(err)
	}
	fake.SetTXTPublished("play.own.net", true)
	if ok, err := g.VerifyProviderClaim(ctx, "play.own.net"); err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	claims, err := g.ProviderCustomDomains(ctx)
	if err != nil || claims["play.own.net"].State != "verified" {
		t.Fatalf("after the writes: %+v, %v", claims, err)
	}
}

func TestRemoteEnsureProviderClaimWaitsForAQueuedRelease(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if err := st.EnqueueOutbox(OutboxOpDeleteCustomDomain, "play.own.net", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.EnsureProviderClaim(context.Background(), "play.own.net"); !errors.Is(err, ErrGatewayChangePending) {
		t.Fatalf("err = %v, want ErrGatewayChangePending", err)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("calls = %v", fake.Calls())
	}
}

// The provider claim is shared by every user here: it stays while any other
// user's claim on the domain may still need it, and is released once once
// none does.
func TestRemoteReleaseKeepsClaimWhileAnotherLocalUserHoldsIt(t *testing.T) {
	g, st, _ := newRemoteTest(t)
	st.claims = []store.CustomDomainClaim{
		{UserID: "a", Domain: "play.own.net", State: store.ClaimVerified},
		{UserID: "b", Domain: "play.own.net", State: store.ClaimPending},
		{UserID: "c", Domain: "play.own.net", State: store.ClaimBlocked},
	}
	open := func() int {
		n := 0
		for _, it := range st.outbox {
			if !it.done && it.op == OutboxOpDeleteCustomDomain && it.domain == "play.own.net" {
				n++
			}
		}
		return n
	}
	if err := g.ReleaseProviderClaim("play.own.net", "a"); err != nil || open() != 0 {
		t.Fatalf("b still pending: err %v, queued %d", err, open())
	}
	st.claims[1].State = store.ClaimPermablocked
	if err := g.ReleaseProviderClaim("play.own.net", ""); err != nil || open() != 0 {
		t.Fatalf("a still verified, nobody excepted: err %v, queued %d", err, open())
	}
	if err := g.ReleaseProviderClaim("play.own.net", "a"); err != nil || open() != 1 {
		t.Fatalf("only a's own claim left: err %v, queued %d", err, open())
	}
	if err := g.ReleaseProviderClaim("play.own.net", "a"); err != nil || open() != 1 {
		t.Fatalf("queued twice: err %v, queued %d", err, open())
	}
}

// verifyOnProvider claims domain on the fake and marks the claim verified, as
// the provider's verifier does once the TXT value shows.
func verifyOnProvider(t *testing.T, g *RemoteGateway, fake *remotegwtest.Fake, domain string) {
	t.Helper()
	if _, err := g.EnsureProviderClaim(context.Background(), domain); err != nil {
		t.Fatal(err)
	}
	fake.SetCustomDomainState(domain, "verified")
}

// The provider refuses a put on an own domain until its claim is verified.
func TestRemoteCreateOnOwnDomainNeedsTheProviderClaim(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if _, err := g.EnsureProviderClaim(context.Background(), "play.own.net"); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateServerRoute(1, "owner", "play.own.net", 25565); !remotegw.IsCode(err, "domain_unverified") {
		t.Fatalf("pending claim: err = %v, want domain_unverified", err)
	}
	if _, ok := st.routes["play.own.net"]; ok {
		t.Fatal("the refused create left its row")
	}
	fake.SetCustomDomainState("play.own.net", "permablocked")
	if err := g.CreateServerRoute(1, "owner", "play.own.net", 25565); !remotegw.IsCode(err, "domain_blocked") {
		t.Fatalf("permablocked: err = %v, want domain_blocked", err)
	}
	fake.SetCustomDomainState("play.own.net", "blocked")
	fake.SetTXTPublished("play.own.net", true)
	fake.SetLiveReverify(true)
	if c, err := g.ProviderCustomDomains(context.Background()); err != nil || c["play.own.net"].State != "blocked" {
		t.Fatalf("cached claim = %+v, %v", c, err)
	}
	if err := g.CreateServerRoute(1, "owner", "play.own.net", 25565); err != nil {
		t.Fatalf("live re-proof: %v", err)
	}
	// The gate proved the claim; the cached "blocked" is gone at once, or the
	// next sweep would delete the route just created.
	if c, err := g.ProviderCustomDomains(context.Background()); err != nil || c["play.own.net"].State != "verified" {
		t.Fatalf("claim after the create = %+v, %v", c, err)
	}
}

// A route delete queued before the release reaches the provider first, and
// the drain drops the cached claim list when the release lands.
func TestOutboxDeletesProviderClaimAfterTheRoute(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	ctx := context.Background()
	verifyOnProvider(t, g, fake, "play.own.net")
	if err := g.CreateServerRoute(1, "owner", "play.own.net", 25565); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteRoute("play.own.net"); err != nil {
		t.Fatal(err)
	}
	if err := g.ReleaseProviderClaim("play.own.net", ""); err != nil {
		t.Fatal(err)
	}
	if ops := openOps(st, "play.own.net"); len(ops) != 2 || ops[0] != OutboxOpDelete || ops[1] != OutboxOpDeleteCustomDomain {
		t.Fatalf("open ops = %v", ops)
	}
	if claims, err := g.ProviderCustomDomains(ctx); err != nil || len(claims) != 1 {
		t.Fatalf("cached claims = %v, %v", claims, err)
	}
	c := remotegw.New(fake.URL(), fake.Key)
	drain := func() (int, int) {
		done, failed, _ := drainOutboxOnce(ctx, st, c, nil, func() time.Time { return outboxT0 }, g)
		return done, failed
	}
	if done, failed := drain(); done != 1 || failed != 0 {
		t.Fatalf("first drain: done %d failed %d", done, failed)
	}
	// Per-domain id order: the claim delete was not tried in the same batch.
	if done, failed := drain(); done != 1 || failed != 0 {
		t.Fatalf("second drain: done %d failed %d", done, failed)
	}
	calls := fake.Calls()
	route, claimDel := -1, -1
	for i, call := range calls {
		switch call {
		case "DELETE /routes/play.own.net":
			route = i
		case "DELETE /custom-domains/play.own.net":
			claimDel = i
		}
	}
	if route < 0 || claimDel < route {
		t.Fatalf("calls = %v, want the route delete before the claim delete", calls)
	}
	if claims, err := g.ProviderCustomDomains(ctx); err != nil || len(claims) != 0 {
		t.Fatalf("claims after the release = %v, %v; the cache was not dropped", claims, err)
	}
}

// A release the provider refuses as in use (here a route of ours on the
// provider with no row here) is finished rather than left open, so it cannot
// hold up every later change to the domain.
func TestOutboxReleaseRefusedInUseIsFinished(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	ctx := context.Background()
	verifyOnProvider(t, g, fake, "kept.own.net")
	kit, err := g.EnsureKit(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	fake.AddRoute(kit.LinkID, "kept.own.net", "mc-"+uuidA, 25565)
	if err := g.ReleaseProviderClaim("kept.own.net", ""); err != nil {
		t.Fatal(err)
	}
	c := remotegw.New(fake.URL(), fake.Key)
	if done, failed, _ := drainOutboxOnce(ctx, st, c, nil, func() time.Time { return outboxT0 }, g); done != 1 || failed != 0 {
		t.Fatalf("drain: done %d failed %d", done, failed)
	}
	if n, _, _ := countOpen(st); n != 0 {
		t.Fatalf("open items = %d, want the refused release finished", n)
	}
	if claims, err := c.CustomDomains(ctx); err != nil || len(claims) != 1 {
		t.Fatalf("provider claims = %+v, %v; the claim in use must stay", claims, err)
	}
	// The domain is usable again at once.
	if _, err := g.EnsureProviderClaim(ctx, "kept.own.net"); err != nil {
		t.Fatalf("ensure after the finished release: %v", err)
	}
}

// An operator route row (no owner, so no local claim, and no delete queued)
// still uses the provider claim: nothing is queued for it.
func TestRemoteReleaseKeepsClaimForAnOperatorRoute(t *testing.T) {
	g, st, _ := newRemoteTest(t)
	st.routes["ops.own.net"] = store.RemoteRoute{Domain: "ops.own.net", ServerUUID: uuidA, NodeID: 7, Port: 25565, CustomDomain: true}
	if err := g.ReleaseProviderClaim("ops.own.net", "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if ops := openOps(st, "ops.own.net"); len(ops) != 0 {
		t.Fatalf("open ops = %v, want none", ops)
	}
	// Once its delete is queued, the release follows it.
	if err := g.DeleteRoute("ops.own.net"); err != nil {
		t.Fatal(err)
	}
	if err := g.ReleaseProviderClaim("ops.own.net", "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if ops := openOps(st, "ops.own.net"); len(ops) != 2 || ops[1] != OutboxOpDeleteCustomDomain {
		t.Fatalf("open ops = %v", ops)
	}
}

func countOpen(st *remoteFakeStore) (int, string, string) {
	n, op, last := 0, "", ""
	for _, it := range st.outbox {
		if !it.done {
			n++
			op, last = it.op, it.lastErr
		}
	}
	return n, op, last
}
