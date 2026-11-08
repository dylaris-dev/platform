package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/store"
)

func routeLive(t *testing.T, g *RedisGateway, domain string) bool {
	t.Helper()
	n, err := g.redis.Exists(context.Background(), "route:"+domain).Result()
	if err != nil {
		t.Fatal(err)
	}
	inIndex, _ := g.redis.SIsMember(context.Background(), "sys:index:routes", domain).Result()
	if (n > 0) != inIndex {
		t.Fatalf("key and index disagree for %s: key=%d index=%v", domain, n, inIndex)
	}
	return n > 0
}

func TestSuspendedRouteIsNotRepublished(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	ctx := context.Background()
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	owner, err := g.SuspendCoreOwnedRoute("play.example.com", "abuse")
	if err != nil || owner != "owner-1" {
		t.Fatalf("suspend = %q, %v", owner, err)
	}
	if routeLive(t, g, "play.example.com") {
		t.Fatal("suspended route is still on the edges")
	}
	rp := &RouteRepublisher{store: g.store, redis: g.redis}
	rp.RunOnce(ctx)
	rp.adoptExisting(ctx)
	if routeLive(t, g, "play.example.com") {
		t.Fatal("the republisher put a suspended route back")
	}
	// A stray key (an old replica, a manual write) is withdrawn on the next tick.
	g.redis.Set(ctx, "route:play.example.com", `{"core_owned":true,"owner_id":"owner-1"}`, 0)
	g.redis.SAdd(ctx, "sys:index:routes", "play.example.com")
	rp.RunOnce(ctx)
	if routeLive(t, g, "play.example.com") {
		t.Fatal("a stray key of a suspended route survived a tick")
	}
	// Somebody else's entry on that domain is not ours to delete.
	g.redis.Set(ctx, "route:play.example.com", `{"core_owned":false}`, 0)
	rp.RunOnce(ctx)
	if n, _ := g.redis.Exists(ctx, "route:play.example.com").Result(); n != 1 {
		t.Fatal("withdraw deleted a managed route's key")
	}
}

func TestSuspendedRouteCannotBeRecreated(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SuspendCoreOwnedRoute("play.example.com", "abuse"); err != nil {
		t.Fatal(err)
	}
	// The owner learns why; a foreign tenant only sees "taken", so another
	// tenant's suspension is not observable.
	for owner, want := range map[string]error{"owner-1": ErrRouteSuspended, "someone-else": ErrRouteDomainTaken} {
		if err := g.CreateRouteViaLink(owner, "play.example.com", "tok2", "10.0.0.1", 25565); !errors.Is(err, want) {
			t.Errorf("%s: create over a suspended route = %v, want %v", owner, err, want)
		}
	}
	if routeLive(t, g, "play.example.com") {
		t.Fatal("a refused create still published the route")
	}
}

func TestUnsuspendRepublishes(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SuspendCoreOwnedRoute("play.example.com", "abuse"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.UnsuspendCoreOwnedRoute("play.example.com"); err != nil {
		t.Fatal(err)
	}
	if !routeLive(t, g, "play.example.com") {
		t.Fatal("unsuspend did not publish the route")
	}
	if r := fs.routes["play.example.com"]; r.SuspendedAt != nil || r.SuspendedReason != "" {
		t.Fatalf("row still suspended: %+v", r)
	}
}

func TestSuspendRefusesManagedAndUnknownRoutes(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	g.redis.Set(context.Background(), "route:managed.example.com", `{"server_uuid":"u"}`, 0)
	if _, err := g.SuspendCoreOwnedRoute("managed.example.com", "x"); !errors.Is(err, ErrRouteNotCoreOwned) {
		t.Errorf("managed = %v, want ErrRouteNotCoreOwned", err)
	}
	if _, err := g.SuspendCoreOwnedRoute("nope.example.com", "x"); !errors.Is(err, ErrRouteNotStored) {
		t.Errorf("unknown = %v, want ErrRouteNotStored", err)
	}
}

func TestSuspendTwiceAndUnsuspendUnsuspendedAreRefused(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	if _, err := g.UnsuspendCoreOwnedRoute("play.example.com"); !errors.Is(err, ErrRouteNotSuspended) {
		t.Errorf("unsuspend of a live route = %v", err)
	}
	if _, err := g.SuspendCoreOwnedRoute("play.example.com", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SuspendCoreOwnedRoute("play.example.com", "second"); !errors.Is(err, ErrRouteAlreadySuspended) {
		t.Errorf("second suspend = %v", err)
	}
	if fs.routes["play.example.com"].SuspendedReason != "first" {
		t.Error("the second suspend overwrote the reason")
	}
}

// An admin suspends between the owner's check and the key being written: the
// create must take its own key back out.
func TestCreateRouteViaLinkWithdrawsWhenSuspendedMidFlight(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	fs.afterUpsert = func(d string) {
		r := fs.routes[d]
		now := time.Now()
		r.SuspendedAt = &now
		fs.routes[d] = r
	}
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "10.0.0.1", 25565); !errors.Is(err, ErrRouteSuspended) {
		t.Fatalf("err = %v, want ErrRouteSuspended", err)
	}
	if routeLive(t, g, "play.example.com") {
		t.Fatal("the key survived the race")
	}
}

func TestSuspendUnpublishesVoiceAndUnsuspendPublishesIt(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	ctx := context.Background()
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	v := store.LinkRouteVoice{Domain: "play.example.com", OwnerID: "owner-1", Region: "eu", Port: 26003, UplinkKey: make([]byte, 32)}
	fs.voice = map[string]store.LinkRouteVoice{v.Domain: v}
	if err := PublishLinkRouteVoice(ctx, rdb, v); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SuspendCoreOwnedRoute(v.Domain, "abuse"); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, VoicePortKey("eu", 26003)).Result(); n != 0 {
		t.Fatal("suspend left the voice port published")
	}
	if _, err := g.UnsuspendCoreOwnedRoute(v.Domain); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, VoicePortKey("eu", 26003)).Result(); n != 1 {
		t.Fatal("unsuspend did not publish the voice port again")
	}
}

func TestReconcileSkipsAndSweepsSuspendedRouteVoice(t *testing.T) {
	_, rdb := newVoiceRedis(t)
	ctx := context.Background()
	rv := testRouteVoice()
	if err := PublishLinkRouteVoice(ctx, rdb, rv); err != nil {
		t.Fatal(err)
	}
	rv.Suspended = true
	svc := NewVoiceService(&routeVoiceFakeStore{routes: []store.LinkRouteVoice{rv}}, rdb)
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, VoicePortKey("eu", 26003)).Result(); n != 0 {
		t.Fatal("the reconcile kept a suspended route's voice port")
	}
}

func TestMergeSuspendedDoesNotListADomainTwice(t *testing.T) {
	live := []PublicRoute{{Domain: "a.example.com"}, {Domain: "b.example.com"}}
	got := MergeSuspended(live, []PublicRoute{{Domain: "b.example.com", Suspended: true, SuspendedReason: "r"}, {Domain: "c.example.com", Suspended: true}})
	if len(got) != 3 || !got[1].Suspended || got[1].SuspendedReason != "r" || got[2].Domain != "c.example.com" {
		t.Fatalf("merged = %+v", got)
	}
}

// Revoking the kit must not lift the takedown: the row survives and a new kit
// cannot re-create the domain.
func TestRevokeKitKeepsSuspendedRouteBlocked(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	tok := g.LinkToken("link-1")
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", tok, "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SuspendCoreOwnedRoute("play.example.com", "abuse"); err != nil {
		t.Fatal(err)
	}
	removed, err := RevokeLinkKitTeardown(context.Background(), fs, g, g.redis, "link-1", "owner-1")
	if err != nil || removed != 0 {
		t.Fatalf("revoke = %d, %v", removed, err)
	}
	if _, ok := fs.routes["play.example.com"]; !ok {
		t.Fatal("the revoke deleted the suspended row")
	}
	if routeLive(t, g, "play.example.com") {
		t.Fatal("the route is published")
	}
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", g.LinkToken("link-2"), "127.0.0.1", 25565); !errors.Is(err, ErrRouteSuspended) {
		t.Fatalf("re-create through a new kit = %v, want ErrRouteSuspended", err)
	}
}

// A foreign tenant must get the same answer for a suspended domain as for any
// taken one, or the suspension of somebody else's route becomes observable.
func TestForeignTenantCannotSeeASuspension(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tok", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SuspendCoreOwnedRoute("play.example.com", "abuse"); err != nil {
		t.Fatal(err)
	}
	err := g.CreateRouteViaLink("someone-else", "play.example.com", "tok2", "10.0.0.1", 25565)
	if !errors.Is(err, ErrRouteDomainTaken) || errors.Is(err, ErrRouteSuspended) {
		t.Fatalf("foreign create = %v, want ErrRouteDomainTaken only", err)
	}
}
