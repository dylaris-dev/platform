package services

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/dnsprov"
	"dylaris-core/store"
)

// fakeReconcileStore holds only what the reconciler reads and writes.
type fakeReconcileStore struct {
	store.Store
	settings  map[string]string
	domains   []store.ServerDomain
	owned     []store.DNSOwnedRecord
	nextOwned int
	accounts  map[int]*store.DNSAccount
	zones     []store.DNSZone
	servers   []models.Server
	nodes     []models.Node
	accErrors []string
	stateSets int
}

func (f *fakeReconcileStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *fakeReconcileStore) ListServerDomains() ([]store.ServerDomain, error) {
	return append([]store.ServerDomain(nil), f.domains...), nil
}
func (f *fakeReconcileStore) SetServerDomainState(id int, state string, problems []string, at time.Time) error {
	f.stateSets++
	for i := range f.domains {
		if f.domains[i].ID == id {
			f.domains[i].State, f.domains[i].Problems, f.domains[i].ObservedAt = state, problems, &at
		}
	}
	return nil
}
func (f *fakeReconcileStore) ListDNSOwnedRecords() ([]store.DNSOwnedRecord, error) {
	return append([]store.DNSOwnedRecord(nil), f.owned...), nil
}

// AddDNSOwnedRecord upserts on (account, zone, name, type, value, port) like
// the real store's unique index.
func (f *fakeReconcileStore) AddDNSOwnedRecord(r store.DNSOwnedRecord) error {
	for i, o := range f.owned {
		if o.AccountID == r.AccountID && o.Zone == r.Zone && o.Name == r.Name && o.Type == r.Type && o.Value == r.Value && o.Port == r.Port {
			f.owned[i].ServerDomainID = r.ServerDomainID
			return nil
		}
	}
	f.nextOwned++
	r.ID = 1000 + f.nextOwned
	f.owned = append(f.owned, r)
	return nil
}
func (f *fakeReconcileStore) DeleteDNSOwnedRecord(id int) error {
	for i, r := range f.owned {
		if r.ID == id {
			f.owned = append(f.owned[:i], f.owned[i+1:]...)
			return nil
		}
	}
	return nil
}
func (f *fakeReconcileStore) GetDNSAccount(id int) (*store.DNSAccount, error) {
	return f.accounts[id], nil
}
func (f *fakeReconcileStore) ListDNSZones() ([]store.DNSZone, error) { return f.zones, nil }
func (f *fakeReconcileStore) SetDNSAccountError(id int, msg string) error {
	f.accErrors = append(f.accErrors, msg)
	if a := f.accounts[id]; a != nil {
		a.LastError = msg
	}
	return nil
}
func (f *fakeReconcileStore) ListAllServers() ([]models.Server, error) { return f.servers, nil }
func (f *fakeReconcileStore) ListNodes() ([]models.Node, error)        { return f.nodes, nil }

// fakeDNSProvider is a zone in memory. SRV names are the bare label, as dnsprov
// hands them out.
type fakeDNSProvider struct {
	recs   map[string][]dnsprov.Record
	calls  []string
	setErr error
	// failZone makes SetRecords fail for that zone only.
	failZone string
	// partial > 0: SetRecords writes that many records, then fails (a provider
	// that writes record by record). afterSet runs once SetRecords wrote.
	partial  int
	afterSet func()
	sets     [][]dnsprov.Record
	dels     [][]dnsprov.Record
}

func (p *fakeDNSProvider) ListZones(context.Context) ([]string, error) { return nil, nil }
func (p *fakeDNSProvider) GetRecords(_ context.Context, zone string) ([]dnsprov.Record, error) {
	p.calls = append(p.calls, "get "+zone)
	return append([]dnsprov.Record(nil), p.recs[zone]...), nil
}
func (p *fakeDNSProvider) SetRecords(_ context.Context, zone string, in []dnsprov.Record) ([]dnsprov.Record, error) {
	p.calls = append(p.calls, "set "+zone)
	if p.failZone == zone {
		return nil, errors.New("500 zone unavailable")
	}
	if p.setErr != nil {
		return nil, p.setErr
	}
	var err error
	if p.partial > 0 && p.partial < len(in) {
		in, err = in[:p.partial], errors.New("500 failed after a partial write")
	}
	p.sets = append(p.sets, in)
	for _, n := range in {
		kept := p.recs[zone][:0]
		for _, r := range p.recs[zone] {
			if r.Name != n.Name || r.Type != n.Type {
				kept = append(kept, r)
			}
		}
		p.recs[zone] = append(kept, n)
	}
	if p.afterSet != nil {
		p.afterSet()
	}
	if err != nil {
		return nil, err
	}
	return in, nil
}
func (p *fakeDNSProvider) DeleteRecords(_ context.Context, zone string, in []dnsprov.Record) error {
	p.calls = append(p.calls, "delete "+zone)
	p.dels = append(p.dels, in)
	for _, d := range in {
		kept := p.recs[zone][:0]
		for _, r := range p.recs[zone] {
			if r != d {
				kept = append(kept, r)
			}
		}
		p.recs[zone] = kept
	}
	return nil
}

const testClusterSecret = "test-cluster-secret"

var testNodeIP = netip.MustParseAddr("203.0.113.10")

func newTestReconciler(t *testing.T) (*ServerDomainReconciler, *fakeReconcileStore, *fakeDNSProvider, *fakeDomainResolver) {
	t.Helper()
	enc, err := EncryptDNSCredentials(testClusterSecret, map[string]string{"api_token": "tok-SECRET-123"})
	if err != nil {
		t.Fatal(err)
	}
	zone := 5
	st := &fakeReconcileStore{
		settings: map[string]string{"feature_dns_updater_enabled": "true", "routing_mode": "ip_port"},
		domains: []store.ServerDomain{{ID: 1, ServerID: 10, FQDN: "survival.example.com",
			Mode: store.DomainModeManaged, ZoneID: &zone, Label: "survival", State: "pending"}},
		accounts: map[int]*store.DNSAccount{3: {ID: 3, Provider: "cloudflare", EncCredentials: enc}},
		zones:    []store.DNSZone{{ID: 5, AccountID: 3, Name: "example.com"}},
		servers:  []models.Server{{ID: 10, NodeID: 2, HostPort: 25650}},
		nodes:    []models.Node{{ID: 2, PublicIP: testNodeIP.String()}},
	}
	prov := &fakeDNSProvider{recs: map[string][]dnsprov.Record{}}
	res := &fakeDomainResolver{}
	r := NewServerDomainReconciler(st, res, testClusterSecret)
	r.newProvider = func(kind string, creds map[string]string) (dnsprov.Provider, error) {
		if creds["api_token"] != "tok-SECRET-123" {
			t.Fatalf("provider got wrong credentials")
		}
		return prov, nil
	}
	return r, st, prov, res
}

func countOwned(st *fakeReconcileStore) int { return len(st.owned) }

func TestReconcileCreatesRecords(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())

	want := []dnsprov.Record{
		{Name: "survival", Type: "A", Value: "203.0.113.10"},
		{Name: "survival", Type: "SRV", Value: "survival.example.com", Port: 25650},
	}
	if len(prov.sets) != 1 || !reflect.DeepEqual(prov.sets[0], want) {
		t.Fatalf("SetRecords = %+v, want one call with %+v", prov.sets, want)
	}
	if countOwned(st) != 2 {
		t.Fatalf("owned rows = %+v, want 2", st.owned)
	}
	for _, o := range st.owned {
		if o.ServerDomainID == nil || *o.ServerDomainID != 1 || o.AccountID != 3 || o.Zone != "example.com" {
			t.Errorf("owned row %+v", o)
		}
	}
	if st.domains[0].State != DomainGrey {
		t.Errorf("state = %q, want grey (DNS does not answer yet)", st.domains[0].State)
	}

	// A second pass with nothing changed writes nothing.
	r.RunOnce(context.Background())
	if len(prov.sets) != 1 || countOwned(st) != 2 {
		t.Fatalf("second pass rewrote: sets=%d owned=%d", len(prov.sets), countOwned(st))
	}
}

func TestReconcileUpdatesOnIPChange(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())
	st.nodes[0].PublicIP = "198.51.100.20"
	r.RunOnce(context.Background())

	if len(prov.sets) != 2 {
		t.Fatalf("sets = %d, want 2", len(prov.sets))
	}
	want := []dnsprov.Record{{Name: "survival", Type: "A", Value: "198.51.100.20"}}
	if !reflect.DeepEqual(prov.sets[1], want) {
		t.Fatalf("second set = %+v, want %+v", prov.sets[1], want)
	}
	if countOwned(st) != 2 {
		t.Fatalf("owned rows = %d, want 2 (reused, no duplicate)", countOwned(st))
	}
}

func TestReconcileRefusesForeignRecord(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	prov.recs["example.com"] = []dnsprov.Record{{Name: "survival", Type: "A", Value: "192.0.2.50"}}
	r.RunOnce(context.Background())

	if len(prov.sets) != 0 || len(prov.dels) != 0 {
		t.Fatalf("wrote despite a foreign record: sets=%+v dels=%+v", prov.sets, prov.dels)
	}
	if countOwned(st) != 0 {
		t.Fatalf("owned rows = %+v", st.owned)
	}
	d := st.domains[0]
	want := []string{"survival.example.com already has an A record that DYLARIS did not create - remove it or pick another name"}
	if d.State != DomainYellow || !reflect.DeepEqual(d.Problems, want) {
		t.Fatalf("state %q %q, want yellow %q", d.State, d.Problems, want)
	}
}

func TestReconcileForeignRecordBlocksOnlyThatDomain(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	zone := 5
	st.domains = append(st.domains, store.ServerDomain{ID: 2, ServerID: 10, FQDN: "play.example.com",
		Mode: store.DomainModeManaged, ZoneID: &zone, Label: "play"})
	prov.recs["example.com"] = []dnsprov.Record{{Name: "survival", Type: "A", Value: "192.0.2.50"}}
	r.RunOnce(context.Background())

	if len(prov.sets) != 1 {
		t.Fatalf("sets = %+v", prov.sets)
	}
	for _, rec := range prov.sets[0] {
		if rec.Name != "play" {
			t.Errorf("wrote %+v for the blocked domain", rec)
		}
	}
}

func TestReconcileNeverTouchesUnknownRecords(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	foreign := []dnsprov.Record{
		{Name: "www", Type: "A", Value: "192.0.2.1"},
		{Name: "mc", Type: "SRV", Value: "mc.example.com", Port: 25570},
		{Name: "", Type: "A", Value: "192.0.2.2"},
	}
	prov.recs["example.com"] = append([]dnsprov.Record(nil), foreign...)

	r.RunOnce(context.Background())
	st.nodes[0].PublicIP = "198.51.100.20"
	r.RunOnce(context.Background())
	st.domains = nil
	for i := range st.owned {
		st.owned[i].ServerDomainID = nil
	}
	r.RunOnce(context.Background())

	for _, batch := range append(prov.sets, prov.dels...) {
		for _, rec := range batch {
			if rec.Name != "survival" {
				t.Errorf("touched %+v", rec)
			}
		}
	}
	if !reflect.DeepEqual(prov.recs["example.com"], foreign) {
		t.Fatalf("zone now %+v, want only %+v", prov.recs["example.com"], foreign)
	}
}

func TestReconcileDeletesOrphanedOwnedRecords(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())
	// The server is deleted: its domain row goes, the ledger rows go NULL.
	st.domains = nil
	for i := range st.owned {
		st.owned[i].ServerDomainID = nil
	}
	r.RunOnce(context.Background())

	if len(prov.dels) != 1 || len(prov.dels[0]) != 2 {
		t.Fatalf("dels = %+v, want one call with A and SRV", prov.dels)
	}
	if countOwned(st) != 0 {
		t.Fatalf("owned rows left: %+v", st.owned)
	}
	if len(prov.recs["example.com"]) != 0 {
		t.Fatalf("zone still has %+v", prov.recs["example.com"])
	}
}

func TestReconcileDropsSRVWhenPortBecomesDefault(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())
	st.servers[0].HostPort = 25565
	r.RunOnce(context.Background())

	if len(prov.dels) != 1 || len(prov.dels[0]) != 1 || prov.dels[0][0].Type != "SRV" {
		t.Fatalf("dels = %+v, want the SRV", prov.dels)
	}
	if countOwned(st) != 1 || st.owned[0].Type != "A" {
		t.Fatalf("owned = %+v", st.owned)
	}
}

func TestReconcileBacksOffZoneAndStatusFollowsDNS(t *testing.T) {
	r, st, prov, res := newTestReconciler(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	prov.setErr = errors.New("403 forbidden: token tok-SECRET-123 invalid")
	// DNS still answers the old address.
	res.a = map[string][]netip.Addr{"survival.example.com": {netip.MustParseAddr("198.51.100.99")}}

	r.RunOnce(context.Background())
	if len(st.accErrors) != 1 || strings.Contains(st.accErrors[0], "tok-SECRET-123") || st.accErrors[0] == "" {
		t.Fatalf("account errors = %q, want one scrubbed message", st.accErrors)
	}
	calls := len(prov.calls)

	now = now.Add(30 * time.Second)
	r.RunOnce(context.Background())
	if len(prov.calls) != calls {
		t.Fatalf("provider called inside the backoff window: %v", prov.calls[calls:])
	}
	d := st.domains[0]
	if d.State != DomainYellow || len(d.Problems) == 0 || !strings.Contains(d.Problems[0], "points to 198.51.100.99") {
		t.Fatalf("state %q %q, want yellow from DNS", d.State, d.Problems)
	}

	// Past the first minute it tries again, fails, and waits two minutes.
	now = now.Add(31 * time.Second)
	r.RunOnce(context.Background())
	if len(prov.calls) == calls {
		t.Fatal("provider not retried after the backoff")
	}
	calls = len(prov.calls)
	now = now.Add(90 * time.Second)
	r.RunOnce(context.Background())
	if len(prov.calls) != calls {
		t.Fatal("backoff did not double")
	}

	// Success clears the error and the backoff.
	prov.setErr = nil
	now = now.Add(time.Minute)
	r.RunOnce(context.Background())
	if last := st.accErrors[len(st.accErrors)-1]; last != "" {
		t.Fatalf("account error not cleared: %q", last)
	}
}

// A failed write leaves no ledger row, so a record someone else creates under
// that name before the retry is foreign and is never overwritten.
func TestReconcileFailedWriteClaimsNothing(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	prov.setErr = errors.New("502 bad gateway")
	r.RunOnce(context.Background())
	if countOwned(st) != 0 {
		t.Fatalf("ledger rows after a failed write: %+v", st.owned)
	}

	foreign := dnsprov.Record{Name: "survival", Type: "A", Value: "192.0.2.50"}
	prov.recs["example.com"] = []dnsprov.Record{foreign}
	prov.setErr = nil
	now = now.Add(2 * time.Minute)
	r.RunOnce(context.Background())

	if len(prov.sets) != 0 || len(prov.dels) != 0 {
		t.Fatalf("overwrote a foreign record: sets=%+v dels=%+v", prov.sets, prov.dels)
	}
	if got := prov.recs["example.com"]; len(got) != 1 || got[0] != foreign {
		t.Fatalf("zone now %+v", got)
	}
	d := st.domains[0]
	want := []string{"survival.example.com already has an A record that DYLARIS did not create - remove it or pick another name"}
	if d.State != DomainYellow || !reflect.DeepEqual(d.Problems, want) {
		t.Fatalf("state %q %q, want yellow %q", d.State, d.Problems, want)
	}
}

// addZone gives account 3 a second zone, "aaa.com" (sorts before example.com),
// with one managed domain "play".
func addZone(st *fakeReconcileStore) {
	zone := 6
	st.zones = append(st.zones, store.DNSZone{ID: 6, AccountID: 3, Name: "aaa.com"})
	st.domains = append(st.domains, store.ServerDomain{ID: 2, ServerID: 10, FQDN: "play.aaa.com",
		Mode: store.DomainModeManaged, ZoneID: &zone, Label: "play"})
}

func countCalls(p *fakeDNSProvider, call string) int {
	n := 0
	for _, c := range p.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestReconcileBackoffIsPerZone(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	addZone(st)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	prov.failZone = "aaa.com"

	r.RunOnce(context.Background())
	if countCalls(prov, "set example.com") != 1 {
		t.Fatalf("example.com not synced after aaa.com failed: %v", prov.calls)
	}
	if last := st.accErrors[len(st.accErrors)-1]; !strings.HasPrefix(last, "aaa.com: ") {
		t.Fatalf("account error = %q, want the failing zone's", last)
	}
	if st.accounts[3].LastError == "" {
		t.Fatal("example.com's success cleared aaa.com's error")
	}

	// 61s: aaa.com retries and fails again (delay 2m); example.com keeps
	// succeeding, which must not reset aaa.com's doubling.
	st.nodes[0].PublicIP = "198.51.100.20"
	now = now.Add(61 * time.Second)
	r.RunOnce(context.Background())
	if countCalls(prov, "set aaa.com") != 2 || countCalls(prov, "set example.com") != 2 {
		t.Fatalf("calls = %v", prov.calls)
	}
	st.nodes[0].PublicIP = "198.51.100.21"
	now = now.Add(90 * time.Second)
	r.RunOnce(context.Background())
	if countCalls(prov, "set aaa.com") != 2 {
		t.Fatalf("aaa.com retried inside its doubled backoff: %v", prov.calls)
	}
	if countCalls(prov, "set example.com") != 3 {
		t.Fatalf("example.com held back: %v", prov.calls)
	}
	if st.accounts[3].LastError == "" {
		t.Fatal("error cleared while aaa.com is still failing")
	}

	// aaa.com recovers: now the account error goes.
	prov.failZone = ""
	now = now.Add(time.Minute)
	r.RunOnce(context.Background())
	if st.accounts[3].LastError != "" {
		t.Fatalf("account error not cleared: %q", st.accounts[3].LastError)
	}
}

func TestReconcileRefusesForeignCNAME(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	addZone(st)
	prov.recs["example.com"] = []dnsprov.Record{{Name: "survival", Type: "CNAME"}}
	r.RunOnce(context.Background())

	for _, batch := range prov.sets {
		for _, rec := range batch {
			if rec.Name == "survival" {
				t.Fatalf("wrote %+v beside a CNAME", rec)
			}
		}
	}
	if countCalls(prov, "set aaa.com") != 1 {
		t.Fatalf("other domain held back: %v", prov.calls)
	}
	d := st.domains[0]
	want := []string{"survival.example.com already has a CNAME record that DYLARIS did not create - remove it or pick another name"}
	if d.State != DomainYellow || !reflect.DeepEqual(d.Problems, want) {
		t.Fatalf("state %q %q, want yellow %q", d.State, d.Problems, want)
	}
}

func TestReconcileRefusesForeignCNAMEOnSRVName(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	prov.recs["example.com"] = []dnsprov.Record{{Name: "_minecraft._tcp.survival", Type: "CNAME"}}
	r.RunOnce(context.Background())
	if len(prov.sets) != 0 {
		t.Fatalf("wrote despite a CNAME on the SRV name: %+v", prov.sets)
	}
	want := []string{"_minecraft._tcp.survival.example.com already has a CNAME record that DYLARIS did not create - remove it or pick another name"}
	if d := st.domains[0]; d.State != DomainYellow || !reflect.DeepEqual(d.Problems, want) {
		t.Fatalf("state %q %q", d.State, d.Problems)
	}
}

func TestReconcileSkipsEmptyLabel(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	st.domains[0].Label = ""
	r.RunOnce(context.Background())
	if len(prov.calls) != 0 {
		t.Fatalf("provider calls for an empty label: %v", prov.calls)
	}
	if d := st.domains[0]; d.State != DomainGrey || !reflect.DeepEqual(d.Problems, []string{"invalid domain label"}) {
		t.Fatalf("state %q %q", d.State, d.Problems)
	}
}

func TestReconcilePausedInGatewayMode(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	st.settings["routing_mode"] = "gateway"
	r.RunOnce(context.Background())
	if len(prov.calls) != 0 {
		t.Fatalf("provider calls in gateway mode: %v", prov.calls)
	}
	if st.domains[0].State != DomainPaused {
		t.Fatalf("state = %q, want paused", st.domains[0].State)
	}
}

func TestReconcileManualDomainOnlyEvaluates(t *testing.T) {
	r, st, prov, res := newTestReconciler(t)
	st.domains = []store.ServerDomain{{ID: 7, ServerID: 10, FQDN: "mc.customer.net", Mode: store.DomainModeManual}}
	res.a = map[string][]netip.Addr{"mc.customer.net": {testNodeIP}}
	res.srv = nil
	r.RunOnce(context.Background())
	if len(prov.calls) != 0 {
		t.Fatalf("provider calls for a manual domain: %v", prov.calls)
	}
	d := st.domains[0]
	want := EvaluateDomain(context.Background(), res, "mc.customer.net", testNodeIP, 25650)
	if d.State != want.State || !reflect.DeepEqual(d.Problems, want.Problems) {
		t.Fatalf("state %q %q, want %q %q", d.State, d.Problems, want.State, want.Problems)
	}
}

func TestReconcileFlagOffIsIdle(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	st.settings["feature_dns_updater_enabled"] = ""
	r.RunOnce(context.Background())
	if len(prov.calls) != 0 || st.stateSets != 0 {
		t.Fatalf("flag off did work: calls=%v stateSets=%d", prov.calls, st.stateSets)
	}
}

func TestReconcileWritesNothingWithoutPublicIP(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	st.nodes[0].PublicIP = "10.0.0.5" // private, and no public address field
	r.RunOnce(context.Background())
	if len(prov.sets) != 0 {
		t.Fatalf("wrote with no public IP: %+v", prov.sets)
	}
	if st.domains[0].State != DomainGrey {
		t.Fatalf("state = %q, want grey", st.domains[0].State)
	}
}

func TestReconcileStateWriteOnlyOnChangeOrStale(t *testing.T) {
	r, st, _, _ := newTestReconciler(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.RunOnce(context.Background())
	n := st.stateSets
	now = now.Add(10 * time.Second)
	r.RunOnce(context.Background())
	if st.stateSets != n {
		t.Fatal("unchanged state rewritten inside the interval")
	}
	now = now.Add(31 * time.Second)
	r.RunOnce(context.Background())
	if st.stateSets != n+1 {
		t.Fatal("stale state not refreshed")
	}
}

func TestReconcileAuditsStatusChange(t *testing.T) {
	r, st, _, res := newTestReconciler(t)
	var events []map[string]any
	r.SetAudit(func(serverID int, event string, details map[string]any) {
		if serverID != 10 || event != "server_domain.status_changed" {
			t.Errorf("audit %d %q", serverID, event)
		}
		events = append(events, details)
	})
	r.RunOnce(context.Background()) // pending -> grey: no event
	if len(events) != 0 {
		t.Fatalf("grey audited: %v", events)
	}
	res.a = map[string][]netip.Addr{"survival.example.com": {netip.MustParseAddr("198.51.100.99")}}
	r.RunOnce(context.Background())
	r.RunOnce(context.Background()) // still yellow: no second event
	want := []map[string]any{{"fqdn": "survival.example.com", "state": DomainYellow}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	_ = st
}

func TestIntervalClampedTo30s(t *testing.T) {
	for raw, want := range map[string]time.Duration{"10": 30 * time.Second, "": 30 * time.Second, "120": 120 * time.Second, "junk": 30 * time.Second,
		"86401": 24 * time.Hour, "999999999999": 24 * time.Hour} {
		st := &fakeReconcileStore{settings: map[string]string{"dns.reconcile_interval_seconds": raw}}
		if got := NewServerDomainReconciler(st, nil, "").Interval(); got != want {
			t.Errorf("Interval(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	in := map[string]string{"api_token": "tok-SECRET-123", "zone": "example.com"}
	enc, err := EncryptDNSCredentials(testClusterSecret, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "tok-SECRET-123") {
		t.Fatal("ciphertext contains the token")
	}
	out, err := DecryptDNSCredentials(testClusterSecret, enc)
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip = %v, %v", out, err)
	}
	if _, err := DecryptDNSCredentials("other-secret", enc); err == nil {
		t.Fatal("decrypt with another secret succeeded")
	}
}

const handChanged = "the A record of survival.example.com was changed at the DNS provider by hand - DYLARIS leaves it alone until the changed record is removed"

// The reviewer's probe: the owner repoints Core's A record by hand, then
// removes the domain. Core must not delete the hand-made value; its untouched
// SRV goes, and the ledger forgets both names.
func TestReconcileLeavesHandRepointedRecordOnRemove(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())
	hand := dnsprov.Record{Name: "survival", Type: "A", Value: "198.51.100.77"}
	for i, rec := range prov.recs["example.com"] {
		if rec.Type == "A" {
			prov.recs["example.com"][i] = hand
		}
	}

	r.RunOnce(context.Background())
	if len(prov.sets) != 1 {
		t.Fatalf("rewrote a record changed by hand: %+v", prov.sets[1:])
	}
	if d := st.domains[0]; d.State != DomainYellow || !reflect.DeepEqual(d.Problems, []string{handChanged}) {
		t.Fatalf("state %q %q, want yellow %q", d.State, d.Problems, handChanged)
	}

	st.domains = nil
	for i := range st.owned {
		st.owned[i].ServerDomainID = nil
	}
	r.RunOnce(context.Background())
	for _, batch := range prov.dels {
		for _, rec := range batch {
			if rec == hand {
				t.Fatalf("deleted the hand-made record %+v", rec)
			}
		}
	}
	if got := prov.recs["example.com"]; !reflect.DeepEqual(got, []dnsprov.Record{hand}) {
		t.Fatalf("zone now %+v, want only the hand-made A", got)
	}
	if countOwned(st) != 0 {
		t.Fatalf("ledger rows left: %+v", st.owned)
	}
}

// A second A the owner adds beside Core's at the same name: SetRecords would
// replace both, so Core writes nothing there and says why.
func TestReconcileSecondARecordAddedByHand(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())
	extra := dnsprov.Record{Name: "survival", Type: "A", Value: "192.0.2.7"}
	prov.recs["example.com"] = append(prov.recs["example.com"], extra)
	st.nodes[0].PublicIP = "198.51.100.20"

	r.RunOnce(context.Background())
	if len(prov.sets) != 1 || len(prov.dels) != 0 {
		t.Fatalf("touched the name beside a hand-made A: sets=%+v dels=%+v", prov.sets[1:], prov.dels)
	}
	if d := st.domains[0]; d.State != DomainYellow || !reflect.DeepEqual(d.Problems, []string{handChanged}) {
		t.Fatalf("state %q %q, want yellow %q", d.State, d.Problems, handChanged)
	}
	found := false
	for _, rec := range prov.recs["example.com"] {
		found = found || rec == extra
	}
	if !found {
		t.Fatal("the hand-made A is gone")
	}
}

// Values are compared normalised on both sides: a provider that reports the
// SRV target in other case with a trailing dot still holds Core's record.
func TestReconcileComparesNormalisedValues(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())
	for i, rec := range prov.recs["example.com"] {
		if rec.Type == "SRV" {
			prov.recs["example.com"][i].Value = "Survival.Example.COM."
		}
	}
	r.RunOnce(context.Background())
	if len(prov.sets) != 1 {
		t.Fatalf("rewrote an unchanged record: %+v", prov.sets[1:])
	}
	if st.domains[0].State == DomainYellow {
		t.Fatalf("own record read as changed: %q", st.domains[0].Problems)
	}
	for _, o := range st.owned {
		if o.Type == "SRV" && (o.Value != "survival.example.com" || o.Port != 25650) {
			t.Errorf("SRV row %+v, want the normalised target and the port", o)
		}
		if o.Type == "A" && (o.Value != "203.0.113.10" || o.Port != 0) {
			t.Errorf("A row %+v", o)
		}
	}
}

// A SetRecords that fails after writing some records: what is in gets its
// ledger row, so the next pass finishes the job instead of calling Core's own
// record foreign forever.
func TestReconcilePartialWriteIsLedgered(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	prov.partial = 1
	r.RunOnce(context.Background())
	if countOwned(st) != 1 || st.owned[0].Type != "A" || st.owned[0].Value != "203.0.113.10" {
		t.Fatalf("ledger after a partial write = %+v, want the A that went in", st.owned)
	}

	prov.partial = 0
	now = now.Add(2 * time.Minute)
	r.RunOnce(context.Background())
	if st.domains[0].State == DomainYellow {
		t.Fatalf("own record read as foreign: %q", st.domains[0].Problems)
	}
	want := []dnsprov.Record{{Name: "survival", Type: "SRV", Value: "survival.example.com", Port: 25650}}
	if last := prov.sets[len(prov.sets)-1]; !reflect.DeepEqual(last, want) {
		t.Fatalf("retry wrote %+v, want only the missing SRV", last)
	}
	if countOwned(st) != 2 {
		t.Fatalf("owned = %+v", st.owned)
	}
}

// A partial write next to a record made by hand ledgers nothing there: the
// name is not provably Core's alone.
func TestReconcilePartialWriteNeverLedgersBesideForeign(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	prov.partial = 1
	// Someone adds an A by hand while the write is running.
	prov.afterSet = func() {
		prov.recs["example.com"] = append(prov.recs["example.com"],
			dnsprov.Record{Name: "survival", Type: "A", Value: "192.0.2.9"})
	}
	r.RunOnce(context.Background())
	if countOwned(st) != 0 {
		t.Fatalf("ledgered beside a foreign record: %+v", st.owned)
	}
}

// A name removed and taken again by a new domain keeps Core's record: the
// orphan row moves to the new domain, nothing is rewritten or deleted.
func TestReconcileRelinksOrphanRowForNewDomain(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	r.RunOnce(context.Background())
	for i := range st.owned {
		st.owned[i].ServerDomainID = nil
	}
	st.domains[0].ID = 9
	r.RunOnce(context.Background())
	if len(prov.sets) != 1 || len(prov.dels) != 0 {
		t.Fatalf("sets=%+v dels=%+v, want nothing new", prov.sets[1:], prov.dels)
	}
	if countOwned(st) != 2 {
		t.Fatalf("owned = %+v", st.owned)
	}
	for _, o := range st.owned {
		if o.ServerDomainID == nil || *o.ServerDomainID != 9 {
			t.Errorf("row %+v not moved to domain 9", o)
		}
	}
}

func TestReconcileStoresClippedError(t *testing.T) {
	r, st, prov, _ := newTestReconciler(t)
	prov.setErr = errors.New(strings.Repeat("x", 2000))
	r.RunOnce(context.Background())
	if len(st.accErrors) != 1 || len(st.accErrors[0]) > dnsprov.MaxErrorLen+3 {
		t.Fatalf("stored error of %d bytes, want at most %d", len(st.accErrors[0]), dnsprov.MaxErrorLen+3)
	}
}
