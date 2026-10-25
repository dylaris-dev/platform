package services

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type routeVoiceFakeStore struct {
	voiceFakeStore
	routes  []store.LinkRouteVoice
	deleted []string
	allowed *bool
}

func (f *routeVoiceFakeStore) ListLinkRouteVoice() ([]store.LinkRouteVoice, error) {
	return f.routes, nil
}

func (f *routeVoiceFakeStore) GetLinkRouteVoice(domain string) (*store.LinkRouteVoice, error) {
	for _, d := range f.deleted {
		if d == domain {
			return nil, nil
		}
	}
	for _, r := range f.routes {
		if r.Domain == domain {
			return &r, nil
		}
	}
	return nil, nil
}

func (f *routeVoiceFakeStore) DeleteLinkRouteVoice(domain string) (bool, error) {
	f.deleted = append(f.deleted, domain)
	return true, nil
}

func (f *routeVoiceFakeStore) GetVoicePermission(scope string) (*bool, error) {
	if strings.HasPrefix(scope, "user:") {
		return f.allowed, nil
	}
	return nil, nil
}

func (f *routeVoiceFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func testRouteVoice() store.LinkRouteVoice {
	return store.LinkRouteVoice{Domain: "r.eu.dylaris.com", OwnerID: "tenant-r", LinkToken: "tok",
		TargetHost: "127.0.0.1", Region: "eu", Port: 26003, TargetPort: VoiceInternalPort, UplinkKey: bytes.Repeat([]byte{0xcd}, 32)}
}

func TestReconcileRepublishesRouteVoiceAndSweepsBeside(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	ctx := context.Background()
	orphan := store.LinkRouteVoice{OwnerID: "gone", Domain: "x.eu.dylaris.com", Region: "eu", Port: 26009, UplinkKey: make([]byte, 32)}
	if err := PublishLinkRouteVoice(ctx, rdb, orphan); err != nil {
		t.Fatal(err)
	}
	rv := testRouteVoice()
	svc := NewVoiceService(&routeVoiceFakeStore{routes: []store.LinkRouteVoice{rv}}, rdb)
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mr.Get("voice:port:eu:26003")
	want := `{"v":1,"server_uuid":"owner:tenant-r:voice:r.eu.dylaris.com","key":"` + strings.Repeat("cd", 32) + `"}`
	if got != want {
		t.Fatalf("route port key:\n got %s\nwant %s", got, want)
	}
	if ok, _ := mr.SIsMember("sys:index:voice:eu", "26003"); !ok {
		t.Fatal("route index member missing")
	}
	if mr.Exists("voice:port:eu:26009") {
		t.Fatal("orphan route port survived")
	}
	// A route has no server key: its link reads the config from Core.
	if keys := mr.Keys(); len(keys) != 2 {
		t.Fatalf("want only the port key and the index, got %v", keys)
	}
}

func TestReconcileRevokesRouteVoiceTheOwnerMayNoLongerHave(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	ctx := context.Background()
	rv := testRouteVoice()
	if err := PublishLinkRouteVoice(ctx, rdb, rv); err != nil {
		t.Fatal(err)
	}
	no := false
	fs := &routeVoiceFakeStore{routes: []store.LinkRouteVoice{rv}, allowed: &no}
	svc := NewVoiceService(fs, rdb)
	svc.SetRevoke(func(context.Context, *models.Server) error { return nil })
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fs.deleted) != 1 || fs.deleted[0] != rv.Domain {
		t.Fatalf("want the route's row deleted, got %v", fs.deleted)
	}
	if mr.Exists("voice:port:eu:26003") {
		t.Fatal("revoked route's port key survived")
	}

	// Allowed again: kept.
	yes := true
	fs2 := &routeVoiceFakeStore{routes: []store.LinkRouteVoice{rv}, allowed: &yes}
	svc2 := NewVoiceService(fs2, rdb)
	svc2.SetRevoke(func(context.Context, *models.Server) error { return nil })
	if err := svc2.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fs2.deleted) != 0 || !mr.Exists("voice:port:eu:26003") {
		t.Fatalf("an allowed route was revoked: deleted=%v", fs2.deleted)
	}
}

func TestLinkRouteVoiceSubjectIsPerDomain(t *testing.T) {
	a := LinkRouteVoiceSubject("u1", "a.eu.dylaris.com")
	b := LinkRouteVoiceSubject("u1", "b.eu.dylaris.com")
	if a == b {
		t.Fatal("two routes of one owner share a wire id")
	}
	if !strings.HasPrefix(a, "owner:u1:") {
		t.Fatalf("subject %q must sit under the owner prefix", a)
	}
}
