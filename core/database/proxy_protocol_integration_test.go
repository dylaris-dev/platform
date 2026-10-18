package database

import (
	"testing"

	"dylaris-core/store"
)

// Phase 36 adds both switches off by default, may run again on an upgraded
// database, and every server read and the route-only table carry the value.
func TestIntegrationProxyProtocolColumns(t *testing.T) {
	db, st := integrationDB(t)
	// InitDB already ran it once; a second boot must be a no-op.
	if err := applyProxyProtocolSchema(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f := newFixture(t, st)

	got, err := st.GetServerByID(f.server.ID)
	if err != nil {
		t.Fatalf("GetServerByID: %v", err)
	}
	if got.ProxyProtocol {
		t.Fatal("a new server must start with the switch off")
	}
	if err := st.SetServerProxyProtocol(f.server.ID, true); err != nil {
		t.Fatalf("SetServerProxyProtocol: %v", err)
	}
	if got, _ := st.GetServerByID(f.server.ID); got == nil || !got.ProxyProtocol {
		t.Fatal("GetServerByID does not read the switch")
	}
	if got, _ := st.GetServerByUUID(f.server.UUID); got == nil || !got.ProxyProtocol {
		t.Fatal("GetServerByUUID does not read the switch")
	}
	list, err := st.ListServersForUser(f.user.ID, false)
	if err != nil {
		t.Fatalf("ListServersForUser: %v", err)
	}
	found := false
	for _, s := range list {
		if s.ID == f.server.ID {
			found = true
			if !s.ProxyProtocol || s.Role != "owner" {
				t.Fatalf("listed server = proxy %v role %q, want true/owner", s.ProxyProtocol, s.Role)
			}
		}
	}
	if !found {
		t.Fatal("fixture server missing from its owner's list")
	}

	domain := uniqueName("pp_") + ".example.test"
	t.Cleanup(func() { st.DeleteCoreLinkRoute(domain) })
	row := store.CoreLinkRoute{Domain: domain, OwnerID: f.user.ID, LinkToken: "tok", TargetHost: "127.0.0.1", TargetPort: 25565, ProxyProtocol: true}
	if err := st.UpsertCoreLinkRoute(row); err != nil {
		t.Fatalf("UpsertCoreLinkRoute: %v", err)
	}
	readRoute := func() store.CoreLinkRoute {
		t.Helper()
		rows, err := st.ListCoreLinkRoutes()
		if err != nil {
			t.Fatalf("ListCoreLinkRoutes: %v", err)
		}
		for _, r := range rows {
			if r.Domain == domain {
				return r
			}
		}
		t.Fatal("route row missing")
		return store.CoreLinkRoute{}
	}
	if !readRoute().ProxyProtocol {
		t.Fatal("route row lost the switch")
	}
	row.ProxyProtocol = false
	if err := st.UpsertCoreLinkRoute(row); err != nil {
		t.Fatal(err)
	}
	if readRoute().ProxyProtocol {
		t.Fatal("upsert did not switch the route off")
	}
}
