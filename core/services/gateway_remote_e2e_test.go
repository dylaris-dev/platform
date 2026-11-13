package services

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
)

// TestRemoteGatewayLifecycleEndToEnd drives one server through the managed
// gateway the way the platform does: create, real-IP on, move to another node,
// delete. Every write goes through the SwitchingGateway in remote mode and the
// outbox drain, against the fake provider, and the routes must follow the
// server without anything left behind on either side.
func TestRemoteGatewayLifecycleEndToEnd(t *testing.T) {
	const domain = "play.example.com"
	for _, tc := range []struct {
		name string
		// rateLimitMove: the provider answers 429 to the first move attempt.
		rateLimitMove bool
	}{
		{name: "clean"},
		{name: "rate limited during the move", rateLimitMove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(prev) })

			rg, st, fake := newRemoteTest(t)
			gw := NewSwitchingGateway(nil, rg, func() (bool, bool) { return true, true })
			c := remotegw.New(fake.URL(), fake.Key)
			ctx := context.Background()
			now := outboxT0
			drain := func() (done, failed int) { return drainAt(ctx, st, c, now) }
			providerRoute := func() (remotegw.Route, bool) {
				t.Helper()
				routes, err := c.Routes(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range routes {
					if r.Domain == domain {
						return r, true
					}
				}
				return remotegw.Route{}, false
			}

			// 1. Create on node 7: the kit is minted and the route posted at once.
			if err := gw.CreateServerRoute(1, "owner", domain, 25565); err != nil {
				t.Fatal(err)
			}
			kitA := st.kits[7]
			r, ok := providerRoute()
			if !ok || r.LinkID != kitA.LinkID || r.TargetHost != "mc-"+uuidA || r.TargetPort != 25565 || r.ProxyProtocol {
				t.Fatalf("after create: provider %+v (found %v), kit %+v", r, ok, kitA)
			}
			if row := st.routes[domain]; row.NodeID != 7 || row.ServerUUID != uuidA {
				t.Fatalf("after create: row %+v", row)
			}

			// 2. Real IP on: queued, then drained.
			st.servers[1].ProxyProtocol = true
			if err := gw.SetServerProxyProtocol(uuidA, true); err != nil {
				t.Fatal(err)
			}
			if r, _ := providerRoute(); r.ProxyProtocol || len(openOutbox(st)) != 1 {
				t.Fatalf("proxy applied before the drain: %+v, outbox %+v", r, openOutbox(st))
			}
			if done, failed := drain(); done != 1 || failed != 0 {
				t.Fatalf("proxy drain: done %d failed %d", done, failed)
			}
			if r, _ := providerRoute(); !r.ProxyProtocol || !st.routes[domain].ProxyProtocol {
				t.Fatalf("after proxy: provider %+v, row %+v", r, st.routes[domain])
			}

			// 3. Move to node 9: a second kit, the route re-posted with it.
			if err := gw.MigrateServerRoutes(1, 9); err != nil {
				t.Fatal(err)
			}
			st.servers[1].NodeID = 9
			kitB, ok := st.kits[9]
			if !ok || kitB.LinkID == kitA.LinkID {
				t.Fatalf("kit for node 9 = %+v (found %v), node 7 has %s", kitB, ok, kitA.LinkID)
			}
			if tc.rateLimitMove {
				// The kit mint above is synchronous; the 429 hits the queued re-post.
				fake.FailNext(1, 429, "rate_limited", 2*time.Second)
				if done, failed := drain(); done != 0 || failed != 1 {
					t.Fatalf("refused move: done %d failed %d", done, failed)
				}
				if r, _ := providerRoute(); r.LinkID != kitA.LinkID || st.routes[domain].NodeID != 7 {
					t.Fatalf("a refused move must change nothing: provider %+v, row %+v", r, st.routes[domain])
				}
				before := len(fake.Calls())
				now = now.Add(5 * time.Second) // inside the backoff: nothing is sent
				if done, failed := drain(); done != 0 || failed != 0 || len(fake.Calls()) != before {
					t.Fatalf("inside the pause: done %d failed %d, calls %v", done, failed, fake.Calls()[before:])
				}
				now = now.Add(outboxBackoffMin)
			}
			if done, failed := drain(); done != 1 || failed != 0 {
				t.Fatalf("move drain: done %d failed %d, outbox %+v", done, failed, openOutbox(st))
			}
			r, _ = providerRoute()
			if r.LinkID != kitB.LinkID || r.TargetHost != "mc-"+uuidA || !r.ProxyProtocol || st.routes[domain].NodeID != 9 {
				t.Fatalf("after move: provider %+v, row %+v", r, st.routes[domain])
			}

			// 4. Delete the server: queued, drained, gone on both sides.
			if err := gw.DeleteServerRoutes(uuidA); err != nil {
				t.Fatal(err)
			}
			delete(st.servers, 1)
			if done, failed := drain(); done != 1 || failed != 0 {
				t.Fatalf("delete drain: done %d failed %d", done, failed)
			}
			if _, ok := providerRoute(); ok {
				t.Fatal("provider route still there after the delete")
			}
			if len(st.routes) != 0 || len(openOutbox(st)) != 0 {
				t.Fatalf("left behind: rows %v, open outbox %+v", st.routes, openOutbox(st))
			}

			var writes []string
			for _, call := range fake.Calls() {
				if !strings.HasPrefix(call, "GET ") {
					writes = append(writes, call)
				}
			}
			want := []string{
				"POST /link-kits",
				"POST /routes " + domain,
				"PATCH /routes/" + domain + "/proxy-protocol",
				"POST /link-kits",
				"POST /routes " + domain,
				"DELETE /routes/" + domain,
			}
			if tc.rateLimitMove {
				// The refused attempt, then the retry.
				want = append(want[:4], append([]string{"POST /routes " + domain}, want[4:]...)...)
			}
			if strings.Join(writes, "\n") != strings.Join(want, "\n") {
				t.Fatalf("provider writes:\n%s\nwant:\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
			}

			// No secret in a URL the provider saw, a log line or a stored error.
			seen := append(fake.Calls(), logged.String())
			for _, it := range st.outbox {
				seen = append(seen, it.lastErr)
			}
			for _, secret := range []string{fake.Key(), kitA.LinkKey, kitB.LinkKey} {
				if secret == "" {
					t.Fatal("a secret under test is empty")
				}
				for _, s := range seen {
					if strings.Contains(s, secret) {
						t.Fatalf("secret leaked into %q", s)
					}
				}
			}
		})
	}
}
