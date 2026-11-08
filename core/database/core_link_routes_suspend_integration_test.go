package database

import (
	"testing"
	"time"

	"dylaris-core/store"
)

func TestIntegrationCoreLinkRouteSuspendColumns(t *testing.T) {
	_, st := integrationDB(t)
	r := store.CoreLinkRoute{Domain: uniqueName("suspend-") + ".example.test", OwnerID: "owner-s", LinkToken: "tok", TargetHost: "127.0.0.1", TargetPort: 25565}
	if err := st.UpsertCoreLinkRoute(r); err != nil {
		t.Fatalf("UpsertCoreLinkRoute: %v", err)
	}
	t.Cleanup(func() { st.DeleteCoreLinkRoute(r.Domain) })

	find := func() store.CoreLinkRoute {
		rows, err := st.ListCoreLinkRoutesByOwner("owner-s")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, x := range rows {
			if x.Domain == r.Domain {
				return x
			}
		}
		t.Fatalf("%s not listed", r.Domain)
		return store.CoreLinkRoute{}
	}
	if got := find(); got.SuspendedAt != nil || got.SuspendedReason != "" {
		t.Fatalf("fresh route is suspended: %+v", got)
	}

	now := time.Now().UTC()
	if ok, err := st.SetCoreLinkRouteSuspended(r.Domain, &now, "abuse"); err != nil || !ok {
		t.Fatalf("suspend = %v, %v", ok, err)
	}
	if got := find(); got.SuspendedAt == nil || got.SuspendedReason != "abuse" {
		t.Fatalf("suspended row = %+v", got)
	}
	// The owner re-saving the route must not lift it.
	if err := st.UpsertCoreLinkRoute(r); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if got := find(); got.SuspendedAt == nil {
		t.Fatal("an upsert cleared the suspension")
	}
	if ok, err := st.SetCoreLinkRouteSuspended(r.Domain, nil, ""); err != nil || !ok {
		t.Fatalf("unsuspend = %v, %v", ok, err)
	}
	if got := find(); got.SuspendedAt != nil || got.SuspendedReason != "" {
		t.Fatalf("unsuspended row = %+v", got)
	}
	if ok, err := st.SetCoreLinkRouteSuspended("nope-"+r.Domain, &now, "x"); err != nil || ok {
		t.Fatalf("unknown domain = %v, %v, want false, nil", ok, err)
	}
}

func TestIntegrationLinkRouteVoiceCarriesSuspension(t *testing.T) {
	_, st := integrationDB(t)
	r := store.CoreLinkRoute{Domain: uniqueName("vsusp-") + ".example.test", OwnerID: "owner-v", LinkToken: "tok", TargetHost: "127.0.0.1", TargetPort: 25565}
	if err := st.UpsertCoreLinkRoute(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DeleteCoreLinkRoute(r.Domain) })
	if _, err := st.AllocateLinkRouteVoice(r.Domain, "eu", 24454, make([]byte, 32), 30000, 30050); err != nil {
		t.Fatalf("AllocateLinkRouteVoice: %v", err)
	}
	get := func() bool {
		v, err := st.GetLinkRouteVoice(r.Domain)
		if err != nil || v == nil {
			t.Fatalf("GetLinkRouteVoice = %v, %v", v, err)
		}
		return v.Suspended
	}
	if get() {
		t.Fatal("fresh route voice reads as suspended")
	}
	now := time.Now().UTC()
	st.SetCoreLinkRouteSuspended(r.Domain, &now, "x")
	if !get() {
		t.Fatal("suspension not visible on the voice row")
	}
}
