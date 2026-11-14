package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const ownerA, ownerB = "tenant-a", "tenant-b"

// newLapseTest: server 1 belongs to tenant A, server 2 to tenant B.
func newLapseTest(t *testing.T) (*RemoteGateway, *remoteFakeStore, *lapseHandle) {
	t.Helper()
	st, fake, c, _ := newOutboxTest(t)
	st.servers[1].OwnerID = ownerA
	st.servers[2].OwnerID = ownerB
	g := NewRemoteGateway(st, func() *remotegw.Client { return c })
	return g, st, &lapseHandle{fake: fake, c: c}
}

type lapseHandle struct {
	fake *remotegwtest.Fake
	c    *remotegw.Client
}

func openOps(st *remoteFakeStore, domain string) []string {
	var out []string
	for _, it := range st.outbox {
		if !it.done && it.domain == domain {
			out = append(out, it.op)
		}
	}
	return out
}

func verifiedOwnClaim(user, domain string) store.CustomDomainClaim {
	return store.CustomDomainClaim{UserID: user, Domain: domain, State: store.ClaimVerified}
}

// reconcileWith runs one sweep with g as the provider-claims source.
func reconcileWith(t *testing.T, st *remoteFakeStore, c *remotegw.Client, g *RemoteGateway) (adopted, repaired, foreign int) {
	t.Helper()
	adopted, repaired, foreign, err := reconcileRemoteRoutes(context.Background(), st, c, g)
	if err != nil {
		t.Fatal(err)
	}
	return adopted, repaired, foreign
}

// ownRow puts an own-domain row of server uuid on node 9 (its kit) with the
// owner's verified local claim.
func ownRow(st *remoteFakeStore, domain, uuid, owner string) {
	st.routes[domain] = store.RemoteRoute{Domain: domain, ServerUUID: uuid, NodeID: 9, Port: 25565, CustomDomain: true}
	if owner != "" {
		st.claims = append(st.claims, verifiedOwnClaim(owner, domain))
	}
}

// Review Focus 2, the stale-cache case: the claim list said verified, the
// provider's claim has lapsed since and its route is gone. The repair is
// refused by the provider's own gate (domain_unverified, or domain_blocked
// for a permanent block), which queues one delete; the next pass puts
// nothing back.
func TestReconcileDeletesOwnDomainRouteTheProviderRefuses(t *testing.T) {
	g, st, h := newLapseTest(t)
	ctx := context.Background()
	for _, d := range []string{"play.own.net", "blocked.own.net"} {
		ownRow(st, d, uuidA, ownerA)
		verifyOnProvider(t, g, h.fake, d)
	}
	if _, err := g.ProviderCustomDomains(ctx); err != nil { // cached as verified
		t.Fatal(err)
	}
	h.fake.SetCustomDomainState("play.own.net", "blocked")
	h.fake.SetCustomDomainState("blocked.own.net", "permablocked")
	if _, r, _ := reconcileWith(t, st, h.c, g); r != 0 {
		t.Fatalf("repaired %d, want 0", r)
	}
	for _, d := range []string{"play.own.net", "blocked.own.net"} {
		if ops := openOps(st, d); len(ops) != 1 || ops[0] != OutboxOpDelete {
			t.Fatalf("%s: open ops = %v, want one delete", d, ops)
		}
	}
	before := countRemoteCalls(h.fake.Calls(), "POST /routes")
	reconcileWith(t, st, h.c, g)
	if n := countRemoteCalls(h.fake.Calls(), "POST /routes"); n != before {
		t.Fatalf("second pass put the route again (%d puts, was %d)", n, before)
	}
}

// Review fix 2: the provider's gate re-proves a lapsed claim live whenever
// its record shows, so a repair would undo the lapse. The sweep reads the
// claim list first and deletes; sweep, drain, sweep never re-create it.
func TestReconcileProviderLapseIsNotPutBack(t *testing.T) {
	g, st, h := newLapseTest(t)
	ctx := context.Background()
	ownRow(st, "play.own.net", uuidA, ownerA)
	verifyOnProvider(t, g, h.fake, "play.own.net")
	h.fake.SetCustomDomainState("play.own.net", "blocked")
	h.fake.SetTXTPublished("play.own.net", true)
	h.fake.SetLiveReverify(true)

	reconcileWith(t, st, h.c, g)
	if ops := openOps(st, "play.own.net"); len(ops) != 1 || ops[0] != OutboxOpDelete {
		t.Fatalf("open ops = %v, want the delete", ops)
	}
	if done, _, _ := drainOutboxOnce(ctx, st, h.c, nil, func() time.Time { return outboxT0 }, g); done != 1 {
		t.Fatalf("drain done %d", done)
	}
	if _, ok := st.routes["play.own.net"]; ok {
		t.Fatal("row left after the delete")
	}
	reconcileWith(t, st, h.c, g)
	if n := countRemoteCalls(h.fake.Calls(), "POST /routes"); n != 0 {
		t.Fatalf("the route was put back %d time(s): %v", n, h.fake.Calls())
	}
	claims, err := h.c.CustomDomains(ctx)
	if err != nil || len(claims) != 1 || claims[0].State != "blocked" {
		t.Fatalf("provider claim = %+v, %v; it must not have been re-proven", claims, err)
	}
}

// A claim list that could not be read is no claims at all: own-domain rows
// are left alone for the pass, hoster rows are repaired as before.
func TestReconcileFailedClaimListChangesNoOwnDomainRow(t *testing.T) {
	g, st, h := newLapseTest(t)
	st.servers[1].NodeID = 9
	ownRow(st, "play.own.net", uuidA, ownerA)
	st.routes["gone.example.com"] = store.RemoteRoute{Domain: "gone.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	// The claims list fails, and the failure is cached (10 s): which row the
	// sweep reaches first is map order, so a FailAfter could hit the hoster
	// row's repair instead.
	h.fake.FailNext(1, 503, "unavailable", 0)
	if _, err := g.ProviderCustomDomains(context.Background()); err == nil {
		t.Fatal("the claims list did not fail")
	}
	if _, r, _ := reconcileWith(t, st, h.c, g); r != 1 {
		t.Fatalf("repaired %d, want the hoster row only", r)
	}
	if len(st.outbox) != 0 {
		t.Fatalf("outbox = %+v, want nothing queued", st.outbox)
	}
	if calls := h.fake.Calls(); countRemoteCalls(calls, "POST /routes") != 1 {
		t.Fatalf("calls = %v, want one put (the hoster row)", calls)
	}
}

// A local-claim lookup that fails is not "no claim": the row is skipped.
func TestReconcileFailedLocalClaimLookupSkipsRow(t *testing.T) {
	g, st, h := newLapseTest(t)
	ownRow(st, "play.own.net", uuidA, ownerA)
	st.claimErr = errors.New("db down")
	reconcileWith(t, st, h.c, g)
	if len(st.outbox) != 0 || countRemoteCalls(h.fake.Calls(), "POST /routes") != 0 || countRemoteCalls(h.fake.Calls(), "GET /custom-domains") != 0 {
		t.Fatalf("outbox %+v, calls %v", st.outbox, h.fake.Calls())
	}
}

func TestReconcileDeletesOwnDomainRouteWithoutLocalClaim(t *testing.T) {
	g, st, h := newLapseTest(t)
	st.servers[1].NodeID = 9
	kit := st.kits[9]
	for _, c := range []struct {
		domain, uuid string
		claim        *store.CustomDomainClaim
	}{
		{"none.own.net", uuidA, nil},
		{"pending.own.net", uuidA, &store.CustomDomainClaim{UserID: ownerA, Domain: "pending.own.net", State: store.ClaimPending}},
		// Tenant B's claim does not carry tenant A's route.
		{"other.own.net", uuidA, &store.CustomDomainClaim{UserID: ownerB, Domain: "other.own.net", State: store.ClaimVerified}},
		{"kept.own.net", uuidA, &store.CustomDomainClaim{UserID: ownerA, Domain: "kept.own.net", State: store.ClaimVerified}},
	} {
		// On the provider, on the server's own node and link, with the
		// provider's claim verified: nothing would be queued but for the
		// local proof.
		st.routes[c.domain] = store.RemoteRoute{Domain: c.domain, ServerUUID: c.uuid, NodeID: 9, Port: 25565, CustomDomain: true}
		verifyOnProvider(t, g, h.fake, c.domain)
		h.fake.AddRoute(kit.LinkID, c.domain, "mc-"+c.uuid, 25565)
		if c.claim != nil {
			st.claims = append(st.claims, *c.claim)
		}
	}
	reconcileWith(t, st, h.c, g)
	for _, d := range []string{"none.own.net", "pending.own.net", "other.own.net"} {
		if ops := openOps(st, d); len(ops) != 1 || ops[0] != OutboxOpDelete {
			t.Errorf("%s: open ops = %v, want only a delete (no move)", d, ops)
		}
	}
	if ops := openOps(st, "kept.own.net"); len(ops) != 0 {
		t.Errorf("kept.own.net with a verified claim got %v", ops)
	}
}

// The operator's own server needs no local claim, but the provider's claim
// still applies.
func TestReconcileOwnerlessServerKeepsOwnDomainRoute(t *testing.T) {
	g, st, h := newLapseTest(t)
	st.servers[1].OwnerID = ""
	st.servers[1].NodeID = 9
	for _, d := range []string{"ops.own.net", "lapsed.own.net"} {
		ownRow(st, d, uuidA, "")
		verifyOnProvider(t, g, h.fake, d)
		h.fake.AddRoute(st.kits[9].LinkID, d, "mc-"+uuidA, 25565)
	}
	h.fake.SetCustomDomainState("lapsed.own.net", "blocked")
	reconcileWith(t, st, h.c, g)
	if ops := openOps(st, "ops.own.net"); len(ops) != 0 {
		t.Fatalf("ops.own.net: open ops = %v, want none", ops)
	}
	if ops := openOps(st, "lapsed.own.net"); len(ops) != 1 || ops[0] != OutboxOpDelete {
		t.Fatalf("lapsed.own.net: open ops = %v, want the delete", ops)
	}
}

// Review fix 2: a queued move on an own-domain row that the provider refuses
// for the domain is finished and the route deleted, not retried every ten
// minutes (each retry a live re-proof on the real provider).
func TestOutboxRefusedMoveOnOwnDomainDeletesTheRoute(t *testing.T) {
	g, st, h := newLapseTest(t)
	ctx := context.Background()
	ownRow(st, "play.own.net", uuidA, ownerA)
	verifyOnProvider(t, g, h.fake, "play.own.net")
	h.fake.SetCustomDomainState("play.own.net", "blocked")
	if err := st.EnqueueOutbox(OutboxOpMove, "play.own.net", OutboxMove{LinkID: st.kits[9].LinkID, NodeID: 9, TargetHost: "mc-" + uuidA, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	drain := func() int {
		done, _, _ := drainOutboxOnce(ctx, st, h.c, nil, func() time.Time { return outboxT0 }, g)
		return done
	}
	if done := drain(); done != 1 {
		t.Fatalf("drain done %d, want the refused move finished", done)
	}
	if ops := openOps(st, "play.own.net"); len(ops) != 1 || ops[0] != OutboxOpDelete {
		t.Fatalf("open ops = %v, want the delete", ops)
	}
	if done := drain(); done != 1 {
		t.Fatalf("second drain done %d", done)
	}
	if _, ok := st.routes["play.own.net"]; ok {
		t.Fatal("row left")
	}
	// The refusal named the provider TXT value; the log must not.
	claims, _ := h.c.CustomDomains(ctx)
	if len(claims) != 1 || claims[0].TXTValue == "" {
		t.Fatalf("claims = %+v", claims)
	}
	if strings.Contains(logs.String(), claims[0].TXTValue) {
		t.Fatalf("the TXT value reached the log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "domain_unverified") {
		t.Fatalf("the log does not name the refusal: %s", logs.String())
	}
}

// Review fix 3: a provider refusal whose message carries a token-like value
// leaves it out of both the log and the stored last_error of own-domain work.
func TestOwnDomainRefusalKeepsTheProviderMessageOut(t *testing.T) {
	const secret = "dylaris-verify=0123456789abcdef0123456789abcdef"
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "code": "forbidden",
			"error": "add a TXT record _dylaris-verify.play.own.net with the value " + secret})
	}))
	t.Cleanup(prov.Close)
	c := remotegw.New(prov.URL, func() string { return "k" })
	st := newRemoteFakeStore()
	st.routes["play.own.net"] = store.RemoteRoute{Domain: "play.own.net", ServerUUID: uuidA, NodeID: 9, Port: 25565, CustomDomain: true}
	if err := st.EnqueueOutbox(OutboxOpProxy, "play.own.net", OutboxProxy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox(OutboxOpDeleteCustomDomain, "gone.own.net", struct{}{}); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	if _, failed, _ := drainOutboxOnce(context.Background(), st, c, nil, func() time.Time { return outboxT0 }, nil); failed != 2 {
		t.Fatalf("failed %d, want 2", failed)
	}
	for _, it := range st.outbox {
		if it.lastErr != "gateway provider answered 403 forbidden" {
			t.Errorf("%s %s last_error = %q", it.op, it.domain, it.lastErr)
		}
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("the value reached the log: %s", logs.String())
	}
}

func TestReconcileAdoptsOwnDomainRouteOnlyWithVerifiedClaim(t *testing.T) {
	_, st, h := newLapseTest(t)
	st.servers[1].NodeID = 9
	st.servers[2].NodeID = 9
	kit := st.kits[9]
	h.fake.AddRoute(kit.LinkID, "proven.own.net", "mc-"+uuidA, 25565)
	h.fake.AddRoute(kit.LinkID, "unproven.own.net", "mc-"+uuidB, 25565)
	h.fake.AddRoute(kit.LinkID, "h.example.com", "mc-"+uuidB, 25565)
	st.claims = []store.CustomDomainClaim{
		verifiedOwnClaim(ownerA, "proven.own.net"),
		// A's claim does not let B's server keep the address.
		verifiedOwnClaim(ownerA, "unproven.own.net"),
	}
	if a, _, _ := reconcileOK(t, st, h.c); a != 2 {
		t.Fatalf("adopted %d, want 2", a)
	}
	if r, ok := st.routes["proven.own.net"]; !ok || !r.CustomDomain || r.ServerUUID != uuidA {
		t.Fatalf("proven row = %+v, %v", r, ok)
	}
	if r, ok := st.routes["h.example.com"]; !ok || r.CustomDomain {
		t.Fatalf("hoster row = %+v, %v", r, ok)
	}
	if _, ok := st.routes["unproven.own.net"]; ok {
		t.Fatal("an own-domain route without its owner's claim was adopted")
	}
	if ops := openOps(st, "unproven.own.net"); len(ops) != 1 || ops[0] != OutboxOpDelete {
		t.Fatalf("unproven: open ops = %v, want a delete", ops)
	}
	if n := countRemoteCalls(h.fake.Calls(), "GET /regions"); n != 1 {
		t.Fatalf("hoster domains read %d times in one pass, want 1", n)
	}
}

// A failed read of the hoster domains adopts nothing and queues nothing.
func TestReconcileAdoptStopsWithoutHosterDomains(t *testing.T) {
	_, st, h := newLapseTest(t)
	st.servers[1].NodeID = 9
	h.fake.AddRoute(st.kits[9].LinkID, "x.own.net", "mc-"+uuidA, 25565)
	h.fake.FailAfter(1, 1, 503, "unavailable", 0)
	if _, _, _, err := reconcileRemoteRoutes(context.Background(), st, h.c, nil); err == nil {
		t.Fatal("want the hoster-domain error")
	}
	if len(st.routes) != 0 || len(st.outbox) != 0 {
		t.Fatalf("rows %v, outbox %v", st.routes, st.outbox)
	}
}

// Rows on hoster domains need no claim and behave as before: a missing
// provider route is put back, a present one is left alone.
func TestReconcileHosterDomainRowsUnchanged(t *testing.T) {
	_, st, h := newLapseTest(t)
	st.servers[1].NodeID = 9
	st.routes["gone.example.com"] = store.RemoteRoute{Domain: "gone.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	st.routes["here.example.com"] = store.RemoteRoute{Domain: "here.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	h.fake.AddRoute(st.kits[9].LinkID, "here.example.com", "mc-"+uuidA, 25565)
	if _, r, _ := reconcileOK(t, st, h.c); r != 1 {
		t.Fatalf("repaired %d, want 1", r)
	}
	if len(st.outbox) != 0 {
		t.Fatalf("outbox = %+v, want nothing queued", st.outbox)
	}
	if n := countRemoteCalls(h.fake.Calls(), "GET /regions"); n != 0 {
		t.Fatalf("hoster domains read %d times with nothing to adopt", n)
	}
}

// Review Focus 2: a local claim that ran out takes its remote route with it,
// through the outbox, and gives the provider claim back. Another tenant's row
// on the domain is not touched, and while that tenant's claim lives the
// provider claim stays.
func TestVerifierRemovesRemoteRouteOnLocalLapse(t *testing.T) {
	remote := func() (bool, bool) { return true, true }
	past := time.Now().Add(-time.Minute)
	for _, c := range []struct {
		name        string
		rowServer   string
		otherClaim  bool
		wantDelete  bool
		wantRelease bool
	}{
		{"own row", uuidA, false, true, true},
		{"another tenant's row", uuidB, true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, st, _ := newLapseTest(t)
			st.routes["mc.example.com"] = store.RemoteRoute{Domain: "mc.example.com", ServerUUID: c.rowServer, NodeID: 7, Port: 25565, CustomDomain: true}
			st.routes["play.other.net"] = store.RemoteRoute{Domain: "play.other.net", ServerUUID: uuidA, NodeID: 7, Port: 25565, CustomDomain: true}
			lapsing := claim(1, "mc.example.com", past)
			lapsing.UserID = ownerA
			st.claims = []store.CustomDomainClaim{lapsing}
			if c.otherClaim {
				st.claims = append(st.claims, verifiedOwnClaim(ownerB, "mc.example.com"))
			}
			fs := &verifierFakeStore{pending: []store.CustomDomainClaim{lapsing}}
			v := NewCustomDomainVerifier(fs, &fakeResolver{}, NewCustomDomainRouteRemover(nil, g, st, st, g, remote))
			v.SetGatewayRemote(remote)
			v.RunOnce(context.Background())

			if len(fs.failed) != 1 {
				t.Fatalf("claim not failed: %v", fs.failed)
			}
			ops := openOps(st, "mc.example.com")
			hasDelete, hasRelease := false, false
			for i, op := range ops {
				hasDelete = hasDelete || op == OutboxOpDelete
				if op == OutboxOpDeleteCustomDomain {
					hasRelease = true
					if c.wantDelete && (i == 0 || ops[0] != OutboxOpDelete) {
						t.Fatalf("release before the route delete: %v", ops)
					}
				}
			}
			if hasDelete != c.wantDelete || hasRelease != c.wantRelease {
				t.Fatalf("open ops = %v, want delete %v release %v", ops, c.wantDelete, c.wantRelease)
			}
			if ops := openOps(st, "play.other.net"); len(ops) != 0 {
				t.Fatalf("another domain got %v", ops)
			}
		})
	}
}

type countingRouteReader struct{ calls int }

func (r *countingRouteReader) GetRemoteRoute(string) (*store.RemoteRoute, error) {
	r.calls++
	return nil, nil
}

type countingReleaser struct{ calls int }

func (r *countingReleaser) ReleaseProviderClaim(string, string) error {
	r.calls++
	return nil
}

// Review Focus 3: in local mode the remover reads Redis and deletes through
// the gateway as before; remote_routes and the provider claim are never
// touched.
func TestVerifierLocalModeUsesRedisPath(t *testing.T) {
	local := func() (bool, bool) { return false, true }
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	seedRoutes(t, rdb, map[string]map[string]interface{}{
		"mc.example.com": {"tunnel_id": "tok", "target_ip": "mc_srv-u1", "target_port": 25565, "server_uuid": "srv-u1"},
	})
	del := &recordingDeleter{}
	reader, releaser := &countingRouteReader{}, &countingReleaser{}
	rm := NewCustomDomainRouteRemover(rdb, del, ownerServers{"u1": {{UUID: "srv-u1"}}}, reader, releaser, local)

	fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", time.Now().Add(-time.Minute))}}
	v := NewCustomDomainVerifier(fs, &fakeResolver{}, rm)
	v.SetGatewayRemote(local)
	before := mr.CommandCount()
	v.RunOnce(context.Background())

	if len(del.deleted) != 1 || del.deleted[0] != "mc.example.com" || len(fs.failed) != 1 {
		t.Fatalf("deleted %v, failed %v", del.deleted, fs.failed)
	}
	if mr.CommandCount() == before {
		t.Fatal("local mode did not read Redis")
	}
	if reader.calls != 0 || releaser.calls != 0 {
		t.Fatalf("local mode read remote_routes %d times, released %d provider claims", reader.calls, releaser.calls)
	}
}

func TestVerifierIdleWhenModeUnknown(t *testing.T) {
	unknown := func() (bool, bool) { return false, false }
	// A nil store panics on any read: the pass must not reach it.
	v := NewCustomDomainVerifier(nil, &fakeResolver{}, &fakeRemover{})
	v.SetGatewayRemote(unknown)
	v.RunOnce(context.Background())

	// The remover asked directly refuses rather than guessing a mode, so a
	// claim is never settled on a removal that did not happen.
	del := &recordingDeleter{}
	reader, releaser := &countingRouteReader{}, &countingReleaser{}
	err := NewCustomDomainRouteRemover(nil, del, ownerServers{}, reader, releaser, unknown).DeleteRoutesForDomain(context.Background(), "u1", "mc.example.com")
	if err == nil || len(del.deleted) != 0 || reader.calls != 0 || releaser.calls != 0 {
		t.Fatalf("err %v, deleted %v, reads %d, releases %d", err, del.deleted, reader.calls, releaser.calls)
	}
}
