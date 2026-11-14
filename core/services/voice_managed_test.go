package services

import (
	"context"
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// reconcileRouteVoice runs one voice reconcile over one published route row
// and reports whether the row was revoked and whether its relay key survived.
func reconcileRouteVoice(t *testing.T, fs *routeVoiceFakeStore) (revoked, keyKept bool) {
	t.Helper()
	mr, rdb := newVoiceRedis(t)
	ctx := context.Background()
	rv := testRouteVoice()
	fs.routes = []store.LinkRouteVoice{rv}
	if err := PublishLinkRouteVoice(ctx, rdb, rv); err != nil {
		t.Fatal(err)
	}
	svc := NewVoiceService(fs, rdb)
	svc.SetRevoke(func(context.Context, *models.Server) error { return nil })
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return len(fs.deleted) > 0, mr.Exists("voice:port:eu:26003")
}

// Global off (no row anywhere): the managed plan keeps voice.
func TestVoiceReconcileKeepsManagedVoiceWithGlobalOff(t *testing.T) {
	revoked, kept := reconcileRouteVoice(t, &routeVoiceFakeStore{global: "false", managed: true})
	if revoked || !kept {
		t.Fatalf("a managed account's voice was revoked: revoked=%v keyKept=%v", revoked, kept)
	}
}

// The store's activate removed the row (the plan ended): the next tick
// revokes, exactly as for a denied permission.
func TestVoiceReconcileRevokesAfterManagedPlanEnds(t *testing.T) {
	revoked, kept := reconcileRouteVoice(t, &routeVoiceFakeStore{global: "false", managed: false})
	if !revoked || kept {
		t.Fatalf("voice outlived the plan: revoked=%v keyKept=%v", revoked, kept)
	}
}

// A database blip must not switch voice off.
func TestVoiceReconcileKeepsVoiceOnAPlanLookupError(t *testing.T) {
	revoked, kept := reconcileRouteVoice(t, &routeVoiceFakeStore{global: "false", managedErr: errors.New("connection reset by peer")})
	if revoked || !kept {
		t.Fatalf("a failed plan lookup revoked voice: revoked=%v keyKept=%v", revoked, kept)
	}
}

// An explicit user deny beats the managed plan; a role deny does not.
func TestVoiceReconcileManagedAgainstDenyRows(t *testing.T) {
	no := false
	for _, c := range []struct {
		name        string
		fs          *routeVoiceFakeStore
		wantRevoked bool
	}{
		{"user deny, managed", &routeVoiceFakeStore{allowed: &no, global: "true", managed: true}, true},
		{"role deny, managed", &routeVoiceFakeStore{role: &no, global: "true", managed: true}, false},
		{"role deny, not managed", &routeVoiceFakeStore{role: &no, global: "true"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			revoked, kept := reconcileRouteVoice(t, c.fs)
			if revoked != c.wantRevoked || kept == c.wantRevoked {
				t.Fatalf("revoked=%v keyKept=%v, want revoked=%v", revoked, kept, c.wantRevoked)
			}
		})
	}
}

func TestVoiceReconcileNonManagedUnchanged(t *testing.T) {
	no, yes := false, true
	for _, c := range []struct {
		name        string
		fs          *routeVoiceFakeStore
		wantRevoked bool
	}{
		{"user denied", &routeVoiceFakeStore{allowed: &no}, true},
		{"user allowed", &routeVoiceFakeStore{allowed: &yes}, false},
		{"global off", &routeVoiceFakeStore{global: "false"}, true},
		{"global on", &routeVoiceFakeStore{global: "true"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			revoked, kept := reconcileRouteVoice(t, c.fs)
			if revoked != c.wantRevoked || kept == c.wantRevoked {
				t.Fatalf("revoked=%v keyKept=%v, want revoked=%v", revoked, kept, c.wantRevoked)
			}
		})
	}
}
