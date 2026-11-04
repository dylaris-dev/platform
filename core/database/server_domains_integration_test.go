package database

import (
	"errors"
	"testing"
	"time"

	"dylaris-core/store"
)

// The store package has no database of its own, so these live next to the
// other Postgres tests and share their fixture. Skipped without
// DYLARIS_TEST_DB_HOST.

func newDNSZone(t *testing.T, st *store.PostgresStore) (accountID int, zone store.DNSZone) {
	t.Helper()
	accountID, err := st.CreateDNSAccount(store.DNSAccount{Name: uniqueName("acc_"), Provider: "test"})
	if err != nil {
		t.Fatalf("CreateDNSAccount: %v", err)
	}
	t.Cleanup(func() { st.DeleteDNSAccount(accountID) })
	name := uniqueName("zone") + ".example.test"
	if err := st.ReplaceDNSZones(accountID, []string{name}, false); err != nil {
		t.Fatalf("ReplaceDNSZones: %v", err)
	}
	zones, err := st.ListDNSZones()
	if err != nil {
		t.Fatalf("ListDNSZones: %v", err)
	}
	for _, z := range zones {
		if z.AccountID == accountID && z.Name == name {
			return accountID, z
		}
	}
	t.Fatalf("zone %s not listed", name)
	return
}

func domainsOf(t *testing.T, st *store.PostgresStore, serverID int) []store.ServerDomain {
	t.Helper()
	ds, err := st.ListServerDomainsByServer(serverID)
	if err != nil {
		t.Fatalf("ListServerDomainsByServer: %v", err)
	}
	return ds
}

func TestIntegrationServerDomainLifecycle(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	accountID, zone := newDNSZone(t, st)

	fqdn := "play." + zone.Name
	id, err := st.CreateServerDomain(store.ServerDomain{
		ServerID: f.server.ID, FQDN: fqdn, Mode: store.DomainModeManaged,
		ZoneID: &zone.ID, Label: "play", CreatedBy: f.user.ID,
	})
	if err != nil {
		t.Fatalf("CreateServerDomain: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := st.SetServerDomainState(id, "problem", []string{"a", "b"}, at); err != nil {
		t.Fatalf("SetServerDomainState: %v", err)
	}
	got := domainsOf(t, st, f.server.ID)
	if len(got) != 1 || got[0].State != "problem" || len(got[0].Problems) != 2 || got[0].Problems[1] != "b" ||
		got[0].ObservedAt == nil || !got[0].ObservedAt.Equal(at) || got[0].ZoneID == nil || *got[0].ZoneID != zone.ID {
		t.Fatalf("round trip lost data: %+v", got)
	}

	if err := st.AddDNSOwnedRecord(store.DNSOwnedRecord{
		AccountID: accountID, Zone: zone.Name, Name: fqdn, Type: "A", ProviderID: "rec-1", ServerDomainID: &id,
	}); err != nil {
		t.Fatalf("AddDNSOwnedRecord: %v", err)
	}

	if err := st.DeleteServer(f.server.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if left := domainsOf(t, st, f.server.ID); len(left) != 0 {
		t.Fatalf("domains survived their server: %+v", left)
	}
	recs, err := st.ListDNSOwnedRecords()
	if err != nil {
		t.Fatalf("ListDNSOwnedRecords: %v", err)
	}
	for _, r := range recs {
		if r.ProviderID == "rec-1" && r.AccountID == accountID {
			if r.ServerDomainID != nil {
				t.Fatalf("owned record still points at domain %d", *r.ServerDomainID)
			}
			return
		}
	}
	t.Fatal("owned record vanished with the domain; the reconciler could never delete it at the provider")
}

func TestIntegrationServerDomainUniqueness(t *testing.T) {
	_, st := integrationDB(t)
	a := newFixture(t, st)
	b := newFixture(t, st)
	_, zone := newDNSZone(t, st)
	add := func(serverID int, fqdn, mode string) error {
		_, err := st.CreateServerDomain(store.ServerDomain{ServerID: serverID, FQDN: fqdn, Mode: mode})
		return err
	}

	managed := "m." + zone.Name
	if err := add(a.server.ID, managed, store.DomainModeManaged); err != nil {
		t.Fatalf("first managed: %v", err)
	}
	if err := add(b.server.ID, managed, store.DomainModeManaged); err == nil {
		t.Error("the same managed FQDN on a second server was accepted")
	}

	manual := uniqueName("man") + ".example.test"
	if err := add(a.server.ID, manual, store.DomainModeManual); err != nil {
		t.Fatalf("manual on server a: %v", err)
	}
	if err := add(b.server.ID, manual, store.DomainModeManual); err != nil {
		t.Errorf("the same manual FQDN on another server was refused: %v", err)
	}
	if err := add(a.server.ID, manual, store.DomainModeManual); err == nil {
		t.Error("the same manual FQDN twice on one server was accepted")
	}
}

func TestIntegrationCountServerDomainsByOwner(t *testing.T) {
	_, st := integrationDB(t)
	mine := newFixture(t, st)
	other := newFixture(t, st)
	_, zone := newDNSZone(t, st)

	for _, d := range []store.ServerDomain{
		{ServerID: mine.server.ID, FQDN: "a." + zone.Name, Mode: store.DomainModeManaged},
		{ServerID: mine.server.ID, FQDN: uniqueName("man") + ".example.test", Mode: store.DomainModeManual},
		{ServerID: other.server.ID, FQDN: "b." + zone.Name, Mode: store.DomainModeManaged},
	} {
		if _, err := st.CreateServerDomain(d); err != nil {
			t.Fatalf("CreateServerDomain %s: %v", d.FQDN, err)
		}
	}
	if n, err := st.CountServerDomainsByOwner(mine.user.ID); err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2 across both modes", n, err)
	}
	if n, err := st.CountServerDomainsByOwner(other.user.ID); err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
}

func TestIntegrationDNSZonesAccessAndLimits(t *testing.T) {
	_, st := integrationDB(t)
	accountID, err := st.CreateDNSAccount(store.DNSAccount{Name: uniqueName("acc_"), Provider: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DeleteDNSAccount(accountID) })

	keep, drop := uniqueName("keep")+".example.test", uniqueName("drop")+".example.test"
	if err := st.ReplaceDNSZones(accountID, []string{keep, drop}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceDNSZones(accountID, []string{keep}, true); err != nil {
		t.Fatal(err)
	}
	var kept *store.DNSZone
	zones, _ := st.ListDNSZones()
	for i, z := range zones {
		if z.AccountID != accountID {
			continue
		}
		if z.Name == drop {
			t.Errorf("zone %s should have been dropped", drop)
		}
		if z.Name == keep {
			kept = &zones[i]
		}
	}
	if kept == nil || !kept.Manual {
		t.Fatalf("kept zone = %+v, want present and manual", kept)
	}

	if err := st.SetDNSZoneAccess(kept.ID, []string{"user:1", "role:2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDNSZoneAccess(kept.ID, []string{"role:2"}); err != nil {
		t.Fatal(err)
	}
	access, _ := st.ListDNSZoneAccess()
	var n int
	for _, a := range access {
		if a.ZoneID == kept.ID {
			n++
			if a.Principal != "role:2" {
				t.Errorf("principal %q survived the replace", a.Principal)
			}
		}
	}
	if n != 1 {
		t.Errorf("access rows = %d, want 1", n)
	}

	scope := uniqueName("scope_")
	if l, err := st.GetDNSSubdomainLimit(scope); l != nil || err != nil {
		t.Fatalf("missing row = %+v, %v; want nil, nil", l, err)
	}
	zero := int64(0)
	for _, max := range []*int64{&zero, nil} {
		if err := st.SetDNSSubdomainLimit(scope, max); err != nil {
			t.Fatal(err)
		}
		l, err := st.GetDNSSubdomainLimit(scope)
		if err != nil || l == nil || (l.Max == nil) != (max == nil) || (max != nil && *l.Max != *max) {
			t.Fatalf("limit round trip for %v: %+v, %v", max, l, err)
		}
	}

	if err := st.SetDNSAccountError(accountID, "boom"); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetDNSAccount(accountID)
	if a == nil || a.LastError != "boom" || a.LastErrorAt == nil {
		t.Fatalf("error not recorded: %+v", a)
	}
	if err := st.SetDNSAccountError(accountID, ""); err != nil {
		t.Fatal(err)
	}
	a, _ = st.GetDNSAccount(accountID)
	if a.LastError != "" || a.LastErrorAt != nil {
		t.Fatalf("error not cleared: %+v", a)
	}
}

// A refresh replaces the listed zones and keeps the ones typed in by hand; a
// domain is stored in the state it is created with, and a duplicate is
// ErrNameTaken rather than the driver's error.
func TestIntegrationManualZonesSurviveRefreshAndDomainState(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	accountID, listed := newDNSZone(t, st)
	typed := uniqueName("typed") + ".example.test"
	if err := st.AddDNSZone(accountID, typed); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDNSZone(accountID, typed); err != nil {
		t.Fatalf("adding the same zone twice: %v", err)
	}
	other := uniqueName("other") + ".example.test"
	if err := st.ReplaceDNSZones(accountID, []string{other}, false); err != nil {
		t.Fatal(err)
	}
	zones, _ := st.ListDNSZones()
	have := map[string]bool{}
	for _, z := range zones {
		if z.AccountID == accountID {
			have[z.Name] = z.Manual
		}
	}
	if manual, ok := have[typed]; !ok || !manual {
		t.Errorf("typed zone after refresh: present=%v manual=%v", ok, manual)
	}
	if _, ok := have[listed.Name]; ok {
		t.Error("a listed zone the refresh no longer lists survived")
	}
	if _, ok := have[other]; !ok || len(have) != 2 {
		t.Errorf("zones after refresh = %v", have)
	}

	d := store.ServerDomain{ServerID: f.server.ID, FQDN: uniqueName("st") + ".example.test", Mode: store.DomainModeManual, State: "grey"}
	if _, err := st.CreateServerDomain(d); err != nil {
		t.Fatal(err)
	}
	if got := domainsOf(t, st, f.server.ID); len(got) != 1 || got[0].State != "grey" {
		t.Errorf("stored %+v, want state grey", got)
	}
	if _, err := st.CreateServerDomain(d); !errors.Is(err, store.ErrNameTaken) {
		t.Errorf("duplicate = %v, want ErrNameTaken", err)
	}
}

func zonesOf(t *testing.T, st *store.PostgresStore, accountID int) map[string]store.DNSZone {
	t.Helper()
	zones, err := st.ListDNSZones()
	if err != nil {
		t.Fatalf("ListDNSZones: %v", err)
	}
	out := map[string]store.DNSZone{}
	for _, z := range zones {
		if z.AccountID == accountID {
			out[z.Name] = z
		}
	}
	return out
}

// A refresh whose listing misses a zone (libdns/cloudflare reads one page)
// must not orphan what uses it: a zone with a domain or an owned record is
// kept and marked not listed, only an unused one goes. Listing it again, or
// adding it by hand, brings it back.
func TestIntegrationRefreshKeepsZonesInUse(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	accountID, err := st.CreateDNSAccount(store.DNSAccount{Name: uniqueName("acc_"), Provider: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DeleteDNSAccount(accountID) })
	withDomain, withRecord, unused, other := uniqueName("dom")+".example.test", uniqueName("rec")+".example.test",
		uniqueName("free")+".example.test", uniqueName("other")+".example.test"
	if err := st.ReplaceDNSZones(accountID, []string{withDomain, withRecord, unused}, false); err != nil {
		t.Fatal(err)
	}
	before := zonesOf(t, st, accountID)
	for _, z := range before {
		if !z.Listed {
			t.Fatalf("fresh zone %s not listed", z.Name)
		}
	}
	zoneID := before[withDomain].ID
	if _, err := st.CreateServerDomain(store.ServerDomain{ServerID: f.server.ID, FQDN: "play." + withDomain,
		Mode: store.DomainModeManaged, ZoneID: &zoneID, Label: "play"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDNSOwnedRecord(store.DNSOwnedRecord{AccountID: accountID, Zone: withRecord, Name: "old", Type: "A", Value: "203.0.113.10"}); err != nil {
		t.Fatal(err)
	}

	if err := st.ReplaceDNSZones(accountID, []string{other}, false); err != nil {
		t.Fatal(err)
	}
	after := zonesOf(t, st, accountID)
	for _, name := range []string{withDomain, withRecord} {
		if z, ok := after[name]; !ok || z.Listed || z.ID != before[name].ID {
			t.Errorf("zone %s after refresh = %+v (present %v), want kept with its id and not listed", name, z, ok)
		}
	}
	if _, ok := after[unused]; ok {
		t.Errorf("unused zone %s survived the refresh", unused)
	}
	if !after[other].Listed {
		t.Errorf("newly listed zone = %+v", after[other])
	}
	if ds := domainsOf(t, st, f.server.ID); len(ds) != 1 || ds[0].ZoneID == nil || *ds[0].ZoneID != zoneID {
		t.Fatalf("domain lost its zone: %+v", ds)
	}

	if err := st.ReplaceDNSZones(accountID, []string{other, withDomain}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDNSZone(accountID, withRecord); err != nil {
		t.Fatal(err)
	}
	again := zonesOf(t, st, accountID)
	if z := again[withDomain]; !z.Listed || z.Manual {
		t.Errorf("re-listed zone = %+v, want listed", z)
	}
	if z := again[withRecord]; !z.Listed || !z.Manual {
		t.Errorf("zone added by hand = %+v, want listed and manual", z)
	}
}

// The one way left for a domain to lose its zone: the account itself goes
// (the handler removes managed domains first, the store still must not take
// the domain row with the zone).
func TestIntegrationDeletedZoneLeavesDomainWithNullZone(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	accountID, zone := newDNSZone(t, st)
	if _, err := st.CreateServerDomain(store.ServerDomain{ServerID: f.server.ID, FQDN: "play." + zone.Name,
		Mode: store.DomainModeManaged, ZoneID: &zone.ID, Label: "play"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDNSAccount(accountID); err != nil {
		t.Fatal(err)
	}
	if ds := domainsOf(t, st, f.server.ID); len(ds) != 1 || ds[0].ZoneID != nil {
		t.Fatalf("domain after its zone went = %+v, want kept with a NULL zone", ds)
	}
}

// The ledger keeps the value Core wrote, and the same record recorded again
// (an orphan row whose name a new domain takes) moves to the new domain
// instead of being duplicated.
func TestIntegrationOwnedRecordValueAndUpsert(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	accountID, zone := newDNSZone(t, st)
	newDomain := func(label string) int {
		id, err := st.CreateServerDomain(store.ServerDomain{ServerID: f.server.ID, FQDN: label + "." + zone.Name,
			Mode: store.DomainModeManaged, ZoneID: &zone.ID, Label: label})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second := newDomain("a"), newDomain("b")
	srv := store.DNSOwnedRecord{AccountID: accountID, Zone: zone.Name, Name: "_minecraft._tcp.play", Type: "SRV",
		Value: "play." + zone.Name, Port: 25650, ServerDomainID: &first}
	for _, r := range []store.DNSOwnedRecord{srv, func() store.DNSOwnedRecord { r := srv; r.ServerDomainID = &second; return r }(),
		func() store.DNSOwnedRecord { r := srv; r.Port = 25651; return r }()} {
		if err := st.AddDNSOwnedRecord(r); err != nil {
			t.Fatalf("AddDNSOwnedRecord %+v: %v", r, err)
		}
	}
	recs, err := st.ListDNSOwnedRecords()
	if err != nil {
		t.Fatal(err)
	}
	byPort := map[uint16]store.DNSOwnedRecord{}
	for _, r := range recs {
		if r.AccountID == accountID {
			byPort[r.Port] = r
		}
	}
	if len(byPort) != 2 {
		t.Fatalf("rows = %+v, want two (one per port)", byPort)
	}
	if r := byPort[25650]; r.Value != srv.Value || r.ServerDomainID == nil || *r.ServerDomainID != second {
		t.Errorf("row = %+v, want value %q moved to domain %d", r, srv.Value, second)
	}
}
