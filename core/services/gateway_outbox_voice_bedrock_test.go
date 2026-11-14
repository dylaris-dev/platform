package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/store"
)

// The voice and bedrock ops: replayed in id order per domain, done when the
// provider has no such address, superseded by a newer delete, backed off on a
// retryable error, paused on a rejected key, left open and visible on a
// refusal. They never write the local columns: the caller already did, and the
// fake store would panic on SetRemoteRouteVoice / SetRemoteRouteEdition.
type outboxEnv struct {
	fake *remotegwtest.Fake
	c    *remotegw.Client
}

func TestOutboxVoiceAndBedrockOps(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, domains ...string) (*remoteFakeStore, outboxEnv) {
		t.Helper()
		st, fake, c, kit := newOutboxTest(t)
		for _, d := range domains {
			fake.AddRoute(kit.LinkID, d, "mc-"+uuidA, 25565)
			// Off here, as every user off leaves the row before it queues; an
			// off queued for a row that is on again is stale and not sent
			// (TestOutboxDrainSkipsAStaleOff).
			st.routes[d] = store.RemoteRoute{Domain: d, ServerUUID: uuidA, NodeID: 9, Port: 25565}
		}
		return st, outboxEnv{fake: fake, c: c}
	}
	enqueue := func(t *testing.T, st *remoteFakeStore, op, d string, payload any) {
		t.Helper()
		if err := st.EnqueueOutbox(op, d, payload); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := func(t *testing.T, st *remoteFakeStore, d string) {
		t.Helper()
		r := st.routes[d]
		if r.VoicePort != nil || r.VoiceMod != "" || r.Edition != "" {
			t.Fatalf("the drain wrote the local record: %+v", r)
		}
	}

	t.Run("in id order per domain", func(t *testing.T) {
		st, f := setup(t, "v.example.com")
		enqueue(t, st, OutboxOpVoice, "v.example.com", OutboxVoice{Enabled: false})
		enqueue(t, st, OutboxOpBedrock, "v.example.com", OutboxBedrock{Java: true, Bedrock: false})
		f.fake.FailNext(1, 503, "internal", 0)
		before := len(f.fake.Calls())
		for _, at := range []time.Duration{0, time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second} {
			drainAt(ctx, st, f.c, outboxT0.Add(at))
		}
		got := strings.Join(f.fake.Calls()[before:], ",")
		want := "PUT /routes/v.example.com/voice,PUT /routes/v.example.com/voice,PUT /routes/v.example.com/bedrock"
		if got != want || len(openOutbox(st)) != 0 {
			t.Fatalf("calls %s, want %s; open %+v", got, want, openOutbox(st))
		}
		unchanged(t, st, "v.example.com")
	})

	t.Run("an address the provider does not have is done", func(t *testing.T) {
		st, f := setup(t)
		st.routes["gone.example.com"] = store.RemoteRoute{Domain: "gone.example.com", ServerUUID: uuidA, NodeID: 9}
		enqueue(t, st, OutboxOpVoice, "gone.example.com", OutboxVoice{Enabled: false})
		enqueue(t, st, OutboxOpBedrock, "gone.example.com", OutboxBedrock{Java: true})
		// The claim takes one item per domain at a time.
		d1, f1 := drainAt(ctx, st, f.c, outboxT0)
		d2, f2 := drainAt(ctx, st, f.c, outboxT0)
		if d1+d2 != 2 || f1+f2 != 0 || len(openOutbox(st)) != 0 {
			t.Fatalf("done %d failed %d open %+v", d1+d2, f1+f2, openOutbox(st))
		}
		unchanged(t, st, "gone.example.com")
	})

	t.Run("a newer delete supersedes them", func(t *testing.T) {
		st, f := setup(t, "d.example.com")
		enqueue(t, st, OutboxOpVoice, "d.example.com", OutboxVoice{Enabled: false})
		enqueue(t, st, OutboxOpBedrock, "d.example.com", OutboxBedrock{Java: true})
		enqueue(t, st, OutboxOpDelete, "d.example.com", struct{}{})
		before := len(f.fake.Calls())
		for i := 0; i < 3; i++ {
			drainAt(ctx, st, f.c, outboxT0)
		}
		if got := strings.Join(f.fake.Calls()[before:], ","); got != "DELETE /routes/d.example.com" || len(openOutbox(st)) != 0 {
			t.Fatalf("calls %s, open %+v", got, openOutbox(st))
		}
	})

	t.Run("a retryable error backs off", func(t *testing.T) {
		st, f := setup(t, "r.example.com")
		enqueue(t, st, OutboxOpBedrock, "r.example.com", OutboxBedrock{Java: true})
		f.fake.FailNext(1, 502, "bad_gateway", 0)
		if done, failed := drainAt(ctx, st, f.c, outboxT0); done != 0 || failed != 1 {
			t.Fatalf("done %d failed %d", done, failed)
		}
		open := openOutbox(st)
		if len(open) != 1 || open[0].next.Before(outboxT0.Add(outboxBackoffMin)) || open[0].attempts != 1 {
			t.Fatalf("open %+v", open)
		}
		if done, _ := drainAt(ctx, st, f.c, outboxT0.Add(outboxBackoffMin)); done != 1 {
			t.Fatalf("after the backoff: done %d", done)
		}
	})

	t.Run("a rejected key pauses the tick", func(t *testing.T) {
		st, f := setup(t, "k1.example.com", "k2.example.com")
		enqueue(t, st, OutboxOpVoice, "k1.example.com", OutboxVoice{Enabled: false})
		enqueue(t, st, OutboxOpVoice, "k2.example.com", OutboxVoice{Enabled: false})
		f.fake.FailNext(1, 401, "", 0)
		before := len(f.fake.Calls())
		_, failed, pause := drainOutboxOnce(ctx, st, f.c, nil, func() time.Time { return outboxT0 }, nil)
		if failed != 1 || !pause.Equal(outboxT0.Add(outboxAuthRetry)) || len(f.fake.Calls()) != before+1 || len(openOutbox(st)) != 2 {
			t.Fatalf("failed %d pause %v calls %v open %+v", failed, pause, f.fake.Calls()[before:], openOutbox(st))
		}
	})

	t.Run("a refusal stays open and visible", func(t *testing.T) {
		st, f := setup(t, "s.example.com")
		f.fake.SetBedrockRefusal("bedrock_disabled")
		f.fake.SetVoiceRefusal("voice_not_allowed")
		// The provider's address is Java only, so this asks to ADD Bedrock.
		enqueue(t, st, OutboxOpBedrock, "s.example.com", OutboxBedrock{Java: true, Bedrock: true})
		if done, failed := drainAt(ctx, st, f.c, outboxT0); done != 0 || failed != 1 {
			t.Fatalf("done %d failed %d", done, failed)
		}
		open := openOutbox(st)
		if len(open) != 1 || !strings.Contains(open[0].lastErr, "409 bedrock_disabled") || !open[0].next.Equal(outboxT0.Add(outboxRefusalRetry)) {
			t.Fatalf("open %+v", open)
		}
		f.fake.SetBedrockRefusal("")
		if done, _ := drainAt(ctx, st, f.c, outboxT0.Add(outboxRefusalRetry)); done != 1 {
			t.Fatalf("after the refusal retry: done %d", done)
		}
		// Voice on with a target port goes the same way.
		enqueue(t, st, OutboxOpVoice, "s.example.com", OutboxVoice{Enabled: true, TargetPort: 24500})
		if _, failed := drainAt(ctx, st, f.c, outboxT0.Add(2*outboxRefusalRetry)); failed != 1 {
			t.Fatalf("voice refusal: failed %d", failed)
		}
		f.fake.SetVoiceRefusal("")
		if done, _ := drainAt(ctx, st, f.c, outboxT0.Add(3*outboxRefusalRetry)); done != 1 {
			t.Fatalf("voice after the refusal: done %d", done)
		}
		routes, _ := f.c.Routes(ctx)
		if len(routes) != 1 || routes[0].Voice == nil || !routes[0].Voice.Enabled || routes[0].Voice.TargetPort != 24500 || routes[0].Bedrock == nil || !routes[0].Bedrock.Bedrock {
			t.Fatalf("provider route = %+v", routes)
		}
		unchanged(t, st, "s.example.com")
	})
}

// ProviderRoute serves the cached route list until a voice or Bedrock op is
// drained here; Players is cached for 5 s.
func TestRemoteGatewayProviderRouteAndPlayersCache(t *testing.T) {
	ctx := context.Background()
	g, st, fake := newRemoteTest(t)
	now := outboxT0
	g.now = func() time.Time { return now }
	kit, err := g.EnsureKit(ctx, 9)
	if err != nil {
		t.Fatal(err)
	}
	fake.AddRoute(kit.LinkID, "c.example.com", "mc-"+uuidA, 25565)
	c := remotegw.New(fake.URL(), fake.Key)

	r, ok, err := g.ProviderRoute(ctx, "c.example.com")
	if err != nil || !ok || r.Voice == nil || r.Voice.Enabled {
		t.Fatalf("first read = %+v %v %v", r, ok, err)
	}
	if _, ok, err := g.ProviderRoute(ctx, "nope.example.com"); err != nil || ok {
		t.Fatalf("unlisted domain = %v %v", ok, err)
	}
	if _, err := c.SetVoice(ctx, "c.example.com", true, 0); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := g.ProviderRoute(ctx, "c.example.com"); r.Voice.Enabled {
		t.Fatal("the route list was not cached")
	}
	if err := st.EnqueueOutbox(OutboxOpBedrock, "c.example.com", OutboxBedrock{Java: true}); err != nil {
		t.Fatal(err)
	}
	if done, _, _ := drainOutboxOnce(ctx, st, c, nil, func() time.Time { return now }, g); done != 1 {
		t.Fatalf("drain done %d", done)
	}
	if r, _, _ := g.ProviderRoute(ctx, "c.example.com"); !r.Voice.Enabled {
		t.Fatal("a drained voice/Bedrock op did not drop the route cache")
	}

	fake.SetPlayers("c.example.com", 2, 1)
	if p, err := g.Players(ctx); err != nil || p.Java != 2 || p.Bedrock != 1 {
		t.Fatalf("players = %+v, %v", p, err)
	}
	fake.SetPlayers("c.example.com", 5, 1)
	if p, _ := g.Players(ctx); p.Java != 2 {
		t.Fatalf("players not cached: %+v", p)
	}
	now = now.Add(remotePlayersTTL)
	if p, _ := g.Players(ctx); p.Java != 5 {
		t.Fatalf("players after the TTL: %+v", p)
	}
}
