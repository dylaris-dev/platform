package database

import (
	"bytes"
	"testing"

	"dylaris-core/store"
)

func linkRoute(t *testing.T, st *store.PostgresStore) store.CoreLinkRoute {
	t.Helper()
	r := store.CoreLinkRoute{Domain: uniqueName("rv_") + ".eu.example.test", OwnerID: "owner-rv", LinkToken: "tok-rv", TargetHost: "127.0.0.1", TargetPort: 25565}
	if err := st.UpsertCoreLinkRoute(r); err != nil {
		t.Fatalf("UpsertCoreLinkRoute: %v", err)
	}
	return r
}

// One pool per region across servers and route-only addresses: each table's
// UNIQUE (region, port) cannot see the other, so the allocators must.
func TestIntegrationLinkRouteVoiceSharesThePoolWithServers(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	srv := newFixture(t, st)
	if got := allocVoice(t, st, srv.server.ID, region, 26000, 26999); got.Port != 26000 {
		t.Fatalf("server got %d, want 26000", got.Port)
	}
	rt := linkRoute(t, st)
	key := bytes.Repeat([]byte{0x42}, 32)
	got, err := st.AllocateLinkRouteVoice(rt.Domain, region, 24454, key, 26000, 26999)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 26001 || got.OwnerID != "owner-rv" || got.LinkToken != "tok-rv" || got.TargetHost != "127.0.0.1" {
		t.Fatalf("route voice = %+v, want port 26001 with the route's owner, link and host", got)
	}
	// And the other way round: a server after the route skips the route's port.
	srv2 := newFixture(t, st)
	if p := allocVoice(t, st, srv2.server.ID, region, 26000, 26999); p.Port != 26002 {
		t.Fatalf("second server got %d, want 26002", p.Port)
	}
	// Same region again keeps port and key; only the target port follows.
	again, err := st.AllocateLinkRouteVoice(rt.Domain, region, 24455, bytes.Repeat([]byte{0x99}, 32), 26000, 26999)
	if err != nil {
		t.Fatal(err)
	}
	if again.Port != 26001 || !bytes.Equal(again.UplinkKey, key) || again.TargetPort != 24455 {
		t.Fatalf("re-allocate = %+v, want port 26001, the first key, target 24455", again)
	}
}

// Deleting the route frees its voice port: the row hangs off the route.
func TestIntegrationLinkRouteVoiceGoesWithTheRoute(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	rt := linkRoute(t, st)
	if _, err := st.AllocateLinkRouteVoice(rt.Domain, region, 24454, make([]byte, 32), 26000, 26999); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteCoreLinkRoute(rt.Domain); err != nil {
		t.Fatal(err)
	}
	if v, err := st.GetLinkRouteVoice(rt.Domain); err != nil || v != nil {
		t.Fatalf("voice of a deleted route = %+v, %v; want nil, nil", v, err)
	}
}
