package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dylaris-core/pkg/leader"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

var outboxT0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

type outboxLeader bool

func (l outboxLeader) IsLeader() bool { return bool(l) }

type outboxLeaderFunc func() bool

func (f outboxLeaderFunc) IsLeader() bool { return f() }

// drainAt drains with a stopped clock and no leader check.
func drainAt(ctx context.Context, st store.Store, c *remotegw.Client, now time.Time) (done, failed int) {
	done, failed, _ = drainOutboxOnce(ctx, st, c, nil, func() time.Time { return now })
	return done, failed
}

func tickAt(l *outboxLoop, now time.Time) {
	l.now = func() time.Time { return now }
	l.tick(context.Background())
}

// newOutboxTest is newRemoteTest plus a kit for node 9 to move routes onto.
func newOutboxTest(t *testing.T) (*remoteFakeStore, *remotegw.Fake, *remotegw.Client, store.RemoteKit) {
	t.Helper()
	g, st, fake := newRemoteTest(t)
	kit, err := g.EnsureKit(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	return st, fake, remotegw.New(fake.URL(), fake.Key), kit
}

func enqueueMove(t *testing.T, st *remoteFakeStore, kit store.RemoteKit, domain string) {
	t.Helper()
	st.routes[domain] = store.RemoteRoute{Domain: domain, ServerUUID: uuidA, NodeID: 7, Port: 25565}
	if err := st.EnqueueOutbox(OutboxOpMove, domain, OutboxMove{LinkID: kit.LinkID, NodeID: 9, TargetHost: "mc-" + uuidA, Port: 25565}); err != nil {
		t.Fatal(err)
	}
}

func openOutbox(st *remoteFakeStore) []remoteOutboxItem {
	var out []remoteOutboxItem
	for _, it := range st.outbox {
		if !it.done {
			out = append(out, it)
		}
	}
	return out
}

func TestOutboxMoveSurvivesRateLimit(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	for i := 0; i < 40; i++ {
		enqueueMove(t, st, kit, fmt.Sprintf("d%02d.example.com", i))
	}
	fake.FailAfter(30, 1, 429, "rate_limited", 2*time.Second)
	ctx := context.Background()

	for _, step := range []struct {
		at           time.Duration
		done, failed int
	}{
		{0, 20, 0},
		{0, 10, 1}, // the 31st call is refused; the tick stops there
		{time.Second, 0, 0},
		{2 * time.Second, 9, 0},  // the untried ones are back when Retry-After ends
		{10 * time.Second, 1, 0}, // the refused one after its backoff
	} {
		before := len(fake.Calls())
		done, failed := drainAt(ctx, st, c, outboxT0.Add(step.at))
		if done != step.done || failed != step.failed {
			t.Fatalf("at +%s: done %d failed %d, want %d %d", step.at, done, failed, step.done, step.failed)
		}
		if step.at == 0 && step.failed == 1 {
			open := openOutbox(st)
			if len(open) != 10 {
				t.Fatalf("open after the 429 = %d, want 10", len(open))
			}
			for _, it := range open {
				if it.next.Before(outboxT0.Add(2*time.Second)) || st.routes[it.domain].NodeID != 7 {
					t.Fatalf("open item %+v, row %+v", it, st.routes[it.domain])
				}
			}
		}
		if step.done == 0 && len(fake.Calls()) != before {
			t.Fatalf("at +%s the provider saw %v", step.at, fake.Calls()[before:])
		}
	}
	if open := openOutbox(st); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	routes, err := c.Routes(ctx)
	if err != nil || len(routes) != 40 {
		t.Fatalf("provider routes = %d, %v", len(routes), err)
	}
	for _, r := range routes {
		if r.LinkID != kit.LinkID || st.routes[r.Domain].NodeID != 9 {
			t.Fatalf("route %+v, row %+v: want both on node 9", r, st.routes[r.Domain])
		}
	}
}

func TestOutboxKeyRejectedStopsTheTick(t *testing.T) {
	st, fake, _, _ := newOutboxTest(t)
	const oldKey = "dyl_rolled_away_key_123"
	c := remotegw.New(fake.URL(), func() string { return oldKey })
	for _, d := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if err := st.EnqueueOutbox(OutboxOpDelete, d, struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	before := len(fake.Calls())
	done, failed := drainAt(context.Background(), st, c, outboxT0)
	if done != 0 || failed != 1 || len(fake.Calls()) != before+1 {
		t.Fatalf("done %d failed %d calls %v", done, failed, fake.Calls()[before:])
	}
	open := openOutbox(st)
	if len(open) != 3 {
		t.Fatalf("open = %+v", open)
	}
	for i, it := range open {
		wantAttempts := 0 // only the one that was sent counts as tried
		if i == 0 {
			wantAttempts = 1
		}
		if !it.next.Equal(outboxT0.Add(time.Minute)) || strings.Contains(it.lastErr, oldKey) || it.attempts != wantAttempts {
			t.Fatalf("item %+v", it)
		}
	}
	if !strings.Contains(open[0].lastErr, "401") {
		t.Fatalf("last_error = %q", open[0].lastErr)
	}
	if drainAt(context.Background(), st, c, outboxT0.Add(30*time.Second)); len(fake.Calls()) != before+1 {
		t.Fatalf("retried within the minute: %v", fake.Calls()[before:])
	}
}

func TestOutboxDeleteNotFoundFinishes(t *testing.T) {
	st, _, c, _ := newOutboxTest(t)
	st.routes["gone.example.com"] = store.RemoteRoute{Domain: "gone.example.com", ServerUUID: uuidA, NodeID: 7}
	if err := st.EnqueueOutbox(OutboxOpDelete, "gone.example.com", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if done, failed := drainAt(context.Background(), st, c, outboxT0); done != 1 || failed != 0 {
		t.Fatalf("done %d failed %d", done, failed)
	}
	if _, ok := st.routes["gone.example.com"]; ok || len(openOutbox(st)) != 0 {
		t.Fatalf("rows %v, open %+v", st.routes, openOutbox(st))
	}
}

func TestOutboxRefusalStaysVisible(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	enqueueMove(t, st, kit, "s.example.com")
	fake.FailNext(1, 409, "route_suspended", 0)
	ctx := context.Background()
	if done, failed := drainAt(ctx, st, c, outboxT0); done != 0 || failed != 1 {
		t.Fatalf("done %d failed %d", done, failed)
	}
	open := openOutbox(st)
	if len(open) != 1 || !strings.Contains(open[0].lastErr, "route_suspended") || st.routes["s.example.com"].NodeID != 7 {
		t.Fatalf("open %+v, row %+v", open, st.routes["s.example.com"])
	}
	before := len(fake.Calls())
	if drainAt(ctx, st, c, outboxT0.Add(9*time.Minute)); len(fake.Calls()) != before {
		t.Fatalf("retried within 10 min: %v", fake.Calls()[before:])
	}
	if done, _ := drainAt(ctx, st, c, outboxT0.Add(10*time.Minute)); done != 1 || st.routes["s.example.com"].NodeID != 9 {
		t.Fatalf("after 10 min: done %d, row %+v", done, st.routes["s.example.com"])
	}
}

func TestOutboxOrderPerDomain(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	// move then proxy: a newer delete would supersede the move (see
	// TestOutboxDeleteSupersedesOlderChanges), so the order is pinned on two
	// changes that are both sent.
	enqueueMove(t, st, kit, "x.example.com")
	if err := st.EnqueueOutbox(OutboxOpProxy, "x.example.com", OutboxProxy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// The move fails and backs off; the proxy, due earlier, must still wait.
	fake.FailNext(1, 503, "unavailable", 0)
	before := len(fake.Calls())
	ctx := context.Background()
	for _, at := range []time.Duration{0, time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second} {
		drainAt(ctx, st, c, outboxT0.Add(at))
	}
	got := strings.Join(fake.Calls()[before:], ",")
	want := "POST /routes x.example.com,POST /routes x.example.com,PATCH /routes/x.example.com/proxy-protocol"
	if got != want {
		t.Fatalf("calls %s, want %s", got, want)
	}
	if r := st.routes["x.example.com"]; r.NodeID != 9 || !r.ProxyProtocol || len(openOutbox(st)) != 0 {
		t.Fatalf("row %+v, open %+v", r, openOutbox(st))
	}
}

func TestOutboxIdleWhenLocal(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	if err := c.PutRoute(context.Background(), kit.LinkID, "p.example.com", "mc-"+uuidA, 25565); err != nil {
		t.Fatal(err)
	}
	st.routes["p.example.com"] = store.RemoteRoute{Domain: "p.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	if err := st.EnqueueOutbox(OutboxOpProxy, "p.example.com", OutboxProxy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before := len(fake.Calls())
	for _, tc := range []struct {
		name          string
		leader        bool
		remote, known bool
	}{
		{"local mode", true, false, true},
		{"mode unknown", true, true, false},
		{"not the leader", false, true, true},
	} {
		l := &outboxLoop{st: st, api: func() *remotegw.Client { return c }, el: outboxLeader(tc.leader),
			mode: func() (bool, bool) { return tc.remote, tc.known }}
		tickAt(l, outboxT0)
		if len(fake.Calls()) != before || st.outbox[0].attempts != 0 || !st.outbox[0].next.IsZero() || st.outbox[0].done {
			t.Fatalf("%s: calls %v, item %+v", tc.name, fake.Calls()[before:], st.outbox[0])
		}
	}
	l := &outboxLoop{st: st, api: func() *remotegw.Client { return c }, el: outboxLeader(true),
		mode: func() (bool, bool) { return true, true }}
	tickAt(l, outboxT0)
	if !st.outbox[0].done || !st.routes["p.example.com"].ProxyProtocol {
		t.Fatalf("remote on: item %+v, row %+v", st.outbox[0], st.routes["p.example.com"])
	}
}

const uuidGone = "2d8b3e4f-8c5f-4e3d-9a91-3c4d5e6f7081"

func reconcileOK(t *testing.T, st *remoteFakeStore, c *remotegw.Client) (adopted, repaired, foreign int) {
	t.Helper()
	adopted, repaired, foreign, err := reconcileRemoteRoutes(context.Background(), st, c)
	if err != nil {
		t.Fatal(err)
	}
	return adopted, repaired, foreign
}

func TestReconcileAdoptsRouteAfterCrash(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	// The provider created both; the Core died before inserting the rows.
	fake.AddRoute(kit.LinkID, "crash.example.com", "mc-"+uuidA, 25570)
	fake.AddRoute(kit.LinkID, "pending.example.com", "mc-"+uuidB, 25565)
	if err := st.EnqueueOutbox(OutboxOpDelete, "pending.example.com", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if a, r, f := reconcileOK(t, st, c); a != 1 || r != 0 || f != 0 {
		t.Fatalf("adopted %d repaired %d foreign %d", a, r, f)
	}
	want := store.RemoteRoute{Domain: "crash.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25570}
	if got := st.routes["crash.example.com"]; got != want {
		t.Fatalf("row %+v, want %+v", got, want)
	}
	if _, ok := st.routes["pending.example.com"]; ok {
		t.Fatal("a domain with an open outbox item must be left to the drain")
	}
	if a, _, _ := reconcileOK(t, st, c); a != 0 {
		t.Fatalf("second pass adopted %d", a)
	}
}

func TestReconcileRepairsMissingProviderRoute(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	st.routes["r.example.com"] = store.RemoteRoute{Domain: "r.example.com", ServerUUID: uuidB, NodeID: 9, Port: 25566, ProxyProtocol: true}
	st.routes["moving.example.com"] = store.RemoteRoute{Domain: "moving.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	if err := st.EnqueueOutbox(OutboxOpMove, "moving.example.com", OutboxMove{LinkID: kit.LinkID, NodeID: 9, TargetHost: "mc-" + uuidA, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	st.routes["nokit.example.com"] = store.RemoteRoute{Domain: "nokit.example.com", ServerUUID: uuidA, NodeID: 7, Port: 25565}
	before := len(fake.Calls())
	if a, r, f := reconcileOK(t, st, c); a != 0 || r != 1 || f != 0 {
		t.Fatalf("adopted %d repaired %d foreign %d", a, r, f)
	}
	if got := strings.Join(fake.Calls()[before:], ","); got != "GET /routes,POST /routes r.example.com" {
		t.Fatalf("calls %s", got)
	}
	routes, err := c.Routes(context.Background())
	if err != nil || len(routes) != 1 || routes[0].TargetHost != "mc-"+uuidB || routes[0].TargetPort != 25566 || routes[0].LinkID != kit.LinkID {
		t.Fatalf("provider routes %+v, %v", routes, err)
	}
	if last := st.outbox[len(st.outbox)-1]; last.op != OutboxOpProxy || last.domain != "r.example.com" || last.payload != (OutboxProxy{Enabled: true}) {
		t.Fatalf("real-IP switch not queued: %+v", st.outbox)
	}
	if _, ok := st.routes["nokit.example.com"]; !ok {
		t.Fatal("a row without a kit is skipped, never deleted")
	}
}

func TestReconcileRemovesRouteOfDeletedServer(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	fake.AddRoute(kit.LinkID, "old.example.com", "mc-"+uuidGone, 25565)
	st.routes["old.example.com"] = store.RemoteRoute{Domain: "old.example.com", ServerUUID: uuidGone, NodeID: 9, Port: 25565}
	for i := 0; i < 2; i++ {
		reconcileOK(t, st, c)
	}
	if len(st.outbox) != 1 || st.outbox[0].op != OutboxOpDelete || st.outbox[0].domain != "old.example.com" {
		t.Fatalf("outbox %+v, want one delete", st.outbox)
	}
	if done, _ := drainAt(context.Background(), st, c, outboxT0); done != 1 {
		t.Fatalf("drain done %d", done)
	}
	routes, err := c.Routes(context.Background())
	if _, ok := st.routes["old.example.com"]; ok || err != nil || len(routes) != 0 {
		t.Fatalf("rows %v, provider %+v, %v", st.routes, routes, err)
	}
}

func TestReconcileLeavesForeignRoutesAlone(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	fake.AddRoute(kit.LinkID, "hand.example.com", "10.0.0.5", 25565)
	fake.AddRoute(kit.LinkID, "other.example.com", "mc-"+uuidGone, 25565) // not a server here
	fake.AddRoute("link-elsewhere", "nokit.example.com", "mc-"+uuidA, 25565)
	before := len(fake.Calls())
	if a, r, f := reconcileOK(t, st, c); a != 0 || r != 0 || f != 3 {
		t.Fatalf("adopted %d repaired %d foreign %d", a, r, f)
	}
	if got := fake.Calls()[before:]; len(got) != 1 || len(st.routes) != 0 || len(st.outbox) != 0 {
		t.Fatalf("calls %v, rows %v, outbox %+v", got, st.routes, st.outbox)
	}
}

func TestReconcileDoesNothingWhenListFails(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	fake.AddRoute(kit.LinkID, "crash.example.com", "mc-"+uuidA, 25565)
	st.routes["r.example.com"] = store.RemoteRoute{Domain: "r.example.com", ServerUUID: uuidB, NodeID: 9, Port: 25565}
	st.routes["old.example.com"] = store.RemoteRoute{Domain: "old.example.com", ServerUUID: uuidGone, NodeID: 9, Port: 25565}
	fake.FailNext(1, 503, "unavailable", 0)
	before := len(fake.Calls())
	if _, _, _, err := reconcileRemoteRoutes(context.Background(), st, c); err == nil {
		t.Fatal("want the list error")
	}
	if got := fake.Calls()[before:]; len(got) != 1 || len(st.routes) != 2 || len(st.outbox) != 0 {
		t.Fatalf("calls %v, rows %v, outbox %+v", got, st.routes, st.outbox)
	}
}

func TestModeFlipKeepsRowsAndSendsNothing(t *testing.T) {
	st, fake, c, _ := newOutboxTest(t)
	st.routes["r.example.com"] = store.RemoteRoute{Domain: "r.example.com", ServerUUID: uuidB, NodeID: 9, Port: 25565}
	st.routes["d.example.com"] = store.RemoteRoute{Domain: "d.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	if err := st.EnqueueOutbox(OutboxOpDelete, "d.example.com", struct{}{}); err != nil {
		t.Fatal(err)
	}
	remote := false
	l := &outboxLoop{st: st, api: func() *remotegw.Client { return c }, el: outboxLeader(true),
		mode: func() (bool, bool) { return remote, true }}
	before := len(fake.Calls())
	for i := 0; i < 3; i++ {
		tickAt(l, outboxT0.Add(time.Duration(i)*time.Minute))
	}
	if got := fake.Calls()[before:]; len(got) != 0 || len(st.routes) != 2 || len(openOutbox(st)) != 1 {
		t.Fatalf("remote off: calls %v, rows %v, open %+v", got, st.routes, openOutbox(st))
	}
	remote = true
	tickAt(l, outboxT0.Add(3*time.Minute))
	got := strings.Join(fake.Calls()[before:], ",")
	if got != "DELETE /routes/d.example.com,GET /routes,POST /routes r.example.com" {
		t.Fatalf("remote on: calls %s", got)
	}
	// Off and on again inside the sweep interval: back on reconciles at once.
	remote = false
	tickAt(l, outboxT0.Add(4*time.Minute))
	remote = true
	before = len(fake.Calls())
	tickAt(l, outboxT0.Add(5*time.Minute))
	if got := fake.Calls()[before:]; len(got) != 1 || got[0] != "GET /routes" {
		t.Fatalf("second flip: calls %v", got)
	}
}

func newActiveLoop(st *remoteFakeStore, c *remotegw.Client) *outboxLoop {
	return &outboxLoop{st: st, api: func() *remotegw.Client { return c }, el: outboxLeader(true),
		mode: func() (bool, bool) { return true, true }}
}

func TestOutboxPauseOutlivesTheBatch(t *testing.T) {
	t.Run("key rejected", func(t *testing.T) {
		st, fake, _, _ := newOutboxTest(t)
		c := remotegw.New(fake.URL(), func() string { return "dyl_rolled_away_key_123" })
		for i := 0; i < 25; i++ {
			if err := st.EnqueueOutbox(OutboxOpDelete, fmt.Sprintf("k%02d.example.com", i), struct{}{}); err != nil {
				t.Fatal(err)
			}
		}
		l := newActiveLoop(st, c)
		before := len(fake.Calls())
		tickAt(l, outboxT0)
		if n := len(fake.Calls()) - before; n != 1 {
			t.Fatalf("first tick: %d calls, want 1 (no reconcile either)", n)
		}
		for _, at := range []time.Duration{5 * time.Second, 30 * time.Second, 55 * time.Second} {
			tickAt(l, outboxT0.Add(at))
		}
		if n := len(fake.Calls()) - before; n != 1 {
			t.Fatalf("paused ticks sent %v", fake.Calls()[before:])
		}
		tickAt(l, outboxT0.Add(61*time.Second))
		if n := len(fake.Calls()) - before; n != 2 {
			t.Fatalf("after the pause: calls %v", fake.Calls()[before:])
		}
	})
	t.Run("rate limited", func(t *testing.T) {
		st, fake, c, kit := newOutboxTest(t)
		for i := 0; i < 25; i++ {
			enqueueMove(t, st, kit, fmt.Sprintf("r%02d.example.com", i))
		}
		fake.FailNext(1, 429, "rate_limited", 2*time.Second)
		l := newActiveLoop(st, c)
		before := len(fake.Calls())
		tickAt(l, outboxT0)
		tickAt(l, outboxT0.Add(time.Second))
		if n := len(fake.Calls()) - before; n != 1 {
			t.Fatalf("inside Retry-After: calls %v", fake.Calls()[before:])
		}
		tickAt(l, outboxT0.Add(3*time.Second))
		if n := len(fake.Calls()) - before; n <= 1 {
			t.Fatal("the tick after Retry-After did not proceed")
		}
	})
}

func TestOutboxBlockedItemsDoNotStarveOtherDomains(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	for i := 0; i < 25; i++ {
		d := fmt.Sprintf("h%02d.example.com", i)
		enqueueMove(t, st, kit, d)
		// The head was refused earlier and waits out its retry.
		st.outbox[len(st.outbox)-1].next = outboxT0.Add(10 * time.Minute)
		st.outbox[len(st.outbox)-1].lastErr = "refused"
		if err := st.EnqueueOutbox(OutboxOpProxy, d, OutboxProxy{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	enqueueMove(t, st, kit, "free.example.com")
	before := len(fake.Calls())
	if done, _ := drainAt(context.Background(), st, c, outboxT0); done != 1 {
		t.Fatalf("done %d, want the unrelated item", done)
	}
	if got := fake.Calls()[before:]; len(got) != 1 || got[0] != "POST /routes free.example.com" {
		t.Fatalf("calls %v", got)
	}
}

func TestOutboxDeleteSupersedesOlderChanges(t *testing.T) {
	st, fake, c, _ := newOutboxTest(t)
	st.routes["s.example.com"] = store.RemoteRoute{Domain: "s.example.com", ServerUUID: uuidA, NodeID: 9}
	if err := st.EnqueueOutbox(OutboxOpProxy, "s.example.com", OutboxProxy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	st.outbox[0].attempts, st.outbox[0].lastErr = 1, "refused" // a refused head, due again
	if err := st.EnqueueOutbox(OutboxOpDelete, "s.example.com", struct{}{}); err != nil {
		t.Fatal(err)
	}
	before := len(fake.Calls())
	// Tick 1 finishes the proxy without a call; the delete was not claimable
	// behind it, so it goes out on tick 2.
	if done, _ := drainAt(context.Background(), st, c, outboxT0); done != 1 || len(fake.Calls()) != before || !st.outbox[0].done {
		t.Fatalf("tick 1: done %d, calls %v, proxy %+v", done, fake.Calls()[before:], st.outbox[0])
	}
	if st.routes["s.example.com"].ProxyProtocol {
		t.Fatal("a superseded proxy must not change the row")
	}
	if done, _ := drainAt(context.Background(), st, c, outboxT0.Add(5*time.Second)); done != 1 {
		t.Fatalf("tick 2: done %d", done)
	}
	if got := fake.Calls()[before:]; len(got) != 1 || got[0] != "DELETE /routes/s.example.com" {
		t.Fatalf("calls %v", got)
	}
}

func TestOutboxProxyOnMissingRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     string
		wantDone bool
	}{
		{"not_found counts as done", "not_found", true},
		{"code-less 404 stays a refusal", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake, c, _ := newOutboxTest(t)
			st.routes["m.example.com"] = store.RemoteRoute{Domain: "m.example.com", ServerUUID: uuidA, NodeID: 9}
			if err := st.EnqueueOutbox(OutboxOpProxy, "m.example.com", OutboxProxy{Enabled: true}); err != nil {
				t.Fatal(err)
			}
			fake.FailNext(1, 404, tc.code, 0)
			drainAt(context.Background(), st, c, outboxT0)
			it := st.outbox[0]
			if it.done != tc.wantDone || st.routes["m.example.com"].ProxyProtocol != tc.wantDone {
				t.Fatalf("item %+v, row %+v", it, st.routes["m.example.com"])
			}
			if !tc.wantDone && (!strings.Contains(it.lastErr, "http_404") || !it.next.Equal(outboxT0.Add(10*time.Minute))) {
				t.Fatalf("refusal: %+v", it)
			}
		})
	}
}

func TestOutboxBatchStopsInsideTheLease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clock func(sent func() int) func() time.Time
		el    func(sent func() int) leader.Election
	}{
		{"budget spent", func(sent func() int) func() time.Time {
			return func() time.Time {
				if sent() >= 3 {
					return outboxT0.Add(61 * time.Second)
				}
				return outboxT0
			}
		}, func(func() int) leader.Election { return nil }},
		{"leadership lost", func(func() int) func() time.Time {
			return func() time.Time { return outboxT0 }
		}, func(sent func() int) leader.Election {
			return outboxLeaderFunc(func() bool { return sent() < 3 })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake, c, kit := newOutboxTest(t)
			for i := 0; i < 10; i++ {
				enqueueMove(t, st, kit, fmt.Sprintf("b%02d.example.com", i))
			}
			before := len(fake.Calls())
			sent := func() int { return len(fake.Calls()) - before }
			done, failed, pause := drainOutboxOnce(context.Background(), st, c, tc.el(sent), tc.clock(sent))
			if done != 3 || failed != 0 || !pause.IsZero() || sent() != 3 {
				t.Fatalf("done %d failed %d pause %v sent %d", done, failed, pause, sent())
			}
			for _, it := range openOutbox(st) {
				if it.attempts != 0 || !it.next.Equal(outboxT0) {
					t.Fatalf("untried item %+v: want due now, no attempt", it)
				}
			}
			if n := len(openOutbox(st)); n != 7 {
				t.Fatalf("open %d, want 7", n)
			}
		})
	}
}

func TestReconcileConfirmsServerIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(st *remoteFakeStore)
	}{
		{"missing from the list, found by uuid", func(st *remoteFakeStore) { st.unlisted = map[string]bool{uuidA: true} }},
		{"lookup error", func(st *remoteFakeStore) {
			st.unlisted = map[string]bool{uuidA: true}
			st.uuidLookupErr = errors.New("db down")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake, c, kit := newOutboxTest(t)
			fake.AddRoute(kit.LinkID, "live.example.com", "mc-"+uuidA, 25565)
			st.routes["live.example.com"] = store.RemoteRoute{Domain: "live.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
			tc.prep(st)
			reconcileOK(t, st, c)
			if len(st.outbox) != 0 {
				t.Fatalf("a live server's route got %+v", st.outbox)
			}
			if _, ok := st.routes["live.example.com"]; !ok {
				t.Fatal("row gone")
			}
		})
	}
}

// A kit revoke the provider did not take on node delete is queued under the
// kit's link id and replayed by the drain; "already gone" finishes it.
func TestOutboxRetriesAFailedKitRevoke(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	kit, err := g.EnsureKit(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	c := remotegw.New(fake.URL(), fake.Key)

	fake.FailNext(1, 503, "http_503", 0)
	g.ForgetNode(context.Background(), 7)
	if _, ok := st.kits[7]; ok {
		t.Fatal("kit row kept")
	}
	open := openOutbox(st)
	if len(open) != 1 || open[0].op != OutboxOpRevokeKit || open[0].domain != kit.LinkID {
		t.Fatalf("outbox = %+v, want one revoke_kit for %s", open, kit.LinkID)
	}
	if done, failed := drainAt(context.Background(), st, c, outboxT0); done != 1 || failed != 0 {
		t.Fatalf("drain = %d done, %d failed", done, failed)
	}
	if n := countRemoteCalls(fake.Calls(), "DELETE /link-kits/"+kit.LinkID); n != 2 {
		t.Fatalf("revokes = %d, want 2 (the failed one and the retry): %v", n, fake.Calls())
	}
	if kits, _ := c.Kits(context.Background()); len(kits) != 0 {
		t.Fatalf("provider still lists %v", kits)
	}

	// Already gone on the provider: done, not retried forever.
	if err := st.EnqueueOutbox(OutboxOpRevokeKit, kit.LinkID, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if done, failed := drainAt(context.Background(), st, c, outboxT0); done != 1 || failed != 0 || len(openOutbox(st)) != 0 {
		t.Fatalf("404 revoke: %d done, %d failed, open %v", done, failed, openOutbox(st))
	}
}

// A Retry-After counts from the refusal, not from the batch start: late in a
// batch the latter has already run out.
func TestOutboxRetryAfterCountsFromTheAnswer(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	enqueueMove(t, st, kit, "late.example.com")
	fake.FailNext(1, 429, "rate_limited", 30*time.Second)
	calls := 0
	clock := func() time.Time {
		calls++
		if calls == 1 {
			return outboxT0 // the batch start
		}
		return outboxT0.Add(50 * time.Second)
	}
	_, failed, pause := drainOutboxOnce(context.Background(), st, c, nil, clock)
	want := outboxT0.Add(80 * time.Second)
	if failed != 1 || !pause.Equal(want) || !st.outbox[0].next.Equal(want) {
		t.Fatalf("failed %d, pause %v, next %v; want both %v", failed, pause, st.outbox[0].next, want)
	}
}

// A route left on another node's kit (the server moved while the gateway was
// local, or a move was refused) is moved by the sweep, once.
func TestReconcileMovesRouteOffTheWrongKit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rowNode uint
	}{
		{"server moved to another node", 7},
		{"row node right, provider link stale", 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake, c, kit := newOutboxTest(t)
			st.servers[1].NodeID = 9
			st.kits[7] = store.RemoteKit{NodeID: 7, LinkID: "link-old"}
			fake.AddRoute("link-old", "drift.example.com", "mc-"+uuidA, 25565)
			st.routes["drift.example.com"] = store.RemoteRoute{Domain: "drift.example.com", ServerUUID: uuidA, NodeID: tc.rowNode, Port: 25565}
			reconcileOK(t, st, c)
			want := OutboxMove{LinkID: kit.LinkID, NodeID: 9, TargetHost: "mc-" + uuidA, Port: 25565}
			if len(st.outbox) != 1 || st.outbox[0].op != OutboxOpMove || st.outbox[0].payload != want {
				t.Fatalf("outbox %+v, want one move %+v", st.outbox, want)
			}
			// While it is open the sweep queues nothing more.
			reconcileOK(t, st, c)
			if len(st.outbox) != 1 {
				t.Fatalf("second sweep with the move open: %+v", st.outbox)
			}
			if done, _ := drainAt(context.Background(), st, c, outboxT0); done != 1 {
				t.Fatalf("drain done %d", done)
			}
			reconcileOK(t, st, c)
			routes, err := c.Routes(context.Background())
			if err != nil || len(routes) != 1 || routes[0].LinkID != kit.LinkID || st.routes["drift.example.com"].NodeID != 9 || len(st.outbox) != 1 {
				t.Fatalf("provider %+v, %v, row %+v, outbox %+v", routes, err, st.routes["drift.example.com"], st.outbox)
			}
		})
	}
}

// The server's real-IP switch wins over what the provider route carries, for an
// adopted route and for one that has a row.
func TestReconcileQueuesTheServersRealIP(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	st.servers[2].NodeID = 9 // server B: real IP on
	fake.AddRoute(kit.LinkID, "adopt.example.com", "mc-"+uuidB, 25565)
	fake.AddRoute(kit.LinkID, "row.example.com", "mc-"+uuidB, 25566)
	st.routes["row.example.com"] = store.RemoteRoute{Domain: "row.example.com", ServerUUID: uuidB, NodeID: 9, Port: 25566}
	if a, _, _ := reconcileOK(t, st, c); a != 1 {
		t.Fatalf("adopted %d", a)
	}
	got := outboxByDomain(st)
	for _, d := range []string{"adopt.example.com", "row.example.com"} {
		if it := got[d]; it.op != OutboxOpProxy || it.payload != (OutboxProxy{Enabled: true}) {
			t.Fatalf("%s: %+v, want the real-IP switch on", d, it)
		}
	}
	if len(st.outbox) != 2 {
		t.Fatalf("outbox %+v", st.outbox)
	}
}

// A's put timed out after the provider took it, A released its claim, B
// claimed the address and put it. A provider list that still shows A's target
// must not hand the address back to A: the row (B) is the truth, and the drift
// move re-puts the route at B.
func TestReconcileNeverHandsAClaimedAddressBack(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	st.servers[2].NodeID = 9
	st.servers[2].ProxyProtocol = false
	fake.AddRoute(kit.LinkID, "contested.example.com", "mc-"+uuidA, 25565)
	b := store.RemoteRoute{Domain: "contested.example.com", ServerUUID: uuidB, NodeID: 9, Port: 25566}
	st.routes[b.Domain] = b
	reconcileOK(t, st, c)
	want := OutboxMove{LinkID: kit.LinkID, NodeID: 9, TargetHost: "mc-" + uuidB, Port: 25566}
	if len(st.outbox) != 1 || st.outbox[0].payload != want {
		t.Fatalf("outbox %+v, want one move to B %+v", st.outbox, want)
	}
	if done, _ := drainAt(context.Background(), st, c, outboxT0); done != 1 {
		t.Fatalf("drain done %d", done)
	}
	routes, err := c.Routes(context.Background())
	if err != nil || len(routes) != 1 || routes[0].TargetHost != "mc-"+uuidB || st.routes[b.Domain] != b {
		t.Fatalf("provider %+v, %v, row %+v; want both naming B", routes, err, st.routes[b.Domain])
	}
}

// B's row is written between the reconcile's row read and its provider read:
// the adopt of A's stale route is a claim that loses, and nothing else is done
// for that domain.
func TestReconcileAdoptNeverOverwritesARow(t *testing.T) {
	st, fake, c, kit := newOutboxTest(t)
	fake.AddRoute(kit.LinkID, "late.example.com", "mc-"+uuidA, 25565)
	b := store.RemoteRoute{Domain: "late.example.com", ServerUUID: uuidB, NodeID: 9, Port: 25566}
	st.afterListRoutes = func() {
		st.mu.Lock()
		st.routes[b.Domain] = b
		st.afterListRoutes = nil
		st.mu.Unlock()
	}
	if a, _, _ := reconcileOK(t, st, c); a != 0 {
		t.Fatalf("adopted %d, want 0", a)
	}
	if st.routes[b.Domain] != b || len(st.outbox) != 0 {
		t.Fatalf("row %+v, outbox %+v; want B's row untouched and nothing queued", st.routes[b.Domain], st.outbox)
	}
}

// A row claimed moments ago may have its create still in flight: no repair
// put and no drift move for it yet.
func TestReconcileLeavesAFreshClaimAlone(t *testing.T) {
	st, fake, c, _ := newOutboxTest(t)
	st.servers[1].NodeID = 9
	st.routes["fresh.example.com"] = store.RemoteRoute{Domain: "fresh.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565, UpdatedAt: time.Now()}
	before := len(fake.Calls())
	if _, r, _ := reconcileOK(t, st, c); r != 0 {
		t.Fatalf("repaired %d, want 0", r)
	}
	if got := fake.Calls()[before:]; len(got) != 1 || len(st.outbox) != 0 {
		t.Fatalf("calls %v, outbox %+v; want only the list", got, st.outbox)
	}
	// Old enough: repaired as before.
	st.routes["fresh.example.com"] = store.RemoteRoute{Domain: "fresh.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565, UpdatedAt: time.Now().Add(-time.Hour)}
	if _, r, _ := reconcileOK(t, st, c); r != 1 {
		t.Fatalf("old row: repaired %d, want 1", r)
	}
}

// The provider omits the link id when its kit lookup fails: unknown, not a
// mismatch, so no move.
func TestReconcileEmptyLinkIDIsNoDrift(t *testing.T) {
	st, fake, c, _ := newOutboxTest(t)
	st.servers[1].NodeID = 9
	fake.AddRoute("", "nolink.example.com", "mc-"+uuidA, 25565)
	st.routes["nolink.example.com"] = store.RemoteRoute{Domain: "nolink.example.com", ServerUUID: uuidA, NodeID: 9, Port: 25565}
	reconcileOK(t, st, c)
	if len(st.outbox) != 0 {
		t.Fatalf("outbox %+v, want nothing", st.outbox)
	}
}
