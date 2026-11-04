package domains

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/dnsprov"
	"dylaris-core/store"
)

const (
	testSecret = "test-cluster-secret"
	testToken  = "tok-SECRET-4711"
)

// fakeStore keeps the domain tables in memory with their unique keys.
type fakeStore struct {
	store.Store
	settings   map[string]string
	servers    map[int]*models.Server
	nodes      map[int]*models.Node
	users      map[string]*models.User
	roles      map[string]int
	domains    []store.ServerDomain
	accounts   map[int]*store.DNSAccount
	zones      []store.DNSZone
	access     []store.DNSZoneAccess
	owned      []store.DNSOwnedRecord
	limits     map[string]*int64
	identity   []*models.AuditEventIdentity
	serverRows []*models.ServerAuditEvent
	nextID     int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		settings: map[string]string{"feature_dns_updater_enabled": "true", "routing_mode": "ip_port"},
		servers: map[int]*models.Server{
			1: {ID: 1, Name: "Survival", OwnerID: "owner", NodeID: 9, HostPort: 25650},
			2: {ID: 2, Name: "Creative", OwnerID: "owner", NodeID: 9, HostPort: 25651},
		},
		nodes:    map[int]*models.Node{9: {ID: 9, PublicIP: "203.0.113.10"}},
		users:    map[string]*models.User{},
		roles:    map[string]int{},
		accounts: map[int]*store.DNSAccount{},
		zones:    []store.DNSZone{{ID: 5, AccountID: 1, Name: "example.com", Listed: true}},
		limits:   map[string]*int64{},
		nextID:   100,
	}
}

func (f *fakeStore) GetSetting(k string) (string, error) {
	v, ok := f.settings[k]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}
func (f *fakeStore) SetSetting(k, v string) error { f.settings[k] = v; return nil }
func (f *fakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeStore) ListAllServers() ([]models.Server, error) {
	out := []models.Server{}
	for _, s := range f.servers {
		out = append(out, *s)
	}
	return out, nil
}
func (f *fakeStore) GetNodeByID(id int) (*models.Node, error) {
	if n, ok := f.nodes[id]; ok {
		return n, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeStore) GetUserByID(id string) (*models.User, error) {
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeStore) GetUserPanelAuthz(id string) (*int, store.CapOverrides, error) {
	if r, ok := f.roles[id]; ok {
		return &r, store.CapOverrides{}, nil
	}
	return nil, store.CapOverrides{}, nil
}
func (f *fakeStore) GetServerAuditState(int) (bool, bool, int, error) { return true, false, 0, nil }
func (f *fakeStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.serverRows = append(f.serverRows, ev)
	return nil
}
func (f *fakeStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	f.identity = append(f.identity, ev)
	return nil
}
func (f *fakeStore) ListServerDomains() ([]store.ServerDomain, error) {
	return append([]store.ServerDomain(nil), f.domains...), nil
}
func (f *fakeStore) ListServerDomainsByServer(id int) ([]store.ServerDomain, error) {
	out := []store.ServerDomain{}
	for _, d := range f.domains {
		if d.ServerID == id {
			out = append(out, d)
		}
	}
	return out, nil
}
func (f *fakeStore) CountServerDomainsByOwner(owner string) (int, error) {
	n := 0
	for _, d := range f.domains {
		if s := f.servers[d.ServerID]; s != nil && s.OwnerID == owner {
			n++
		}
	}
	return n, nil
}
func (f *fakeStore) CreateServerDomain(d store.ServerDomain) (int, error) {
	for _, o := range f.domains {
		if (o.ServerID == d.ServerID && o.FQDN == d.FQDN) ||
			(o.FQDN == d.FQDN && o.Mode == store.DomainModeManaged && d.Mode == store.DomainModeManaged) {
			return 0, store.ErrNameTaken
		}
	}
	f.nextID++
	d.ID = f.nextID
	f.domains = append(f.domains, d)
	return d.ID, nil
}
func (f *fakeStore) DeleteServerDomain(id int) error {
	for i, d := range f.domains {
		if d.ID == id {
			f.domains = append(f.domains[:i], f.domains[i+1:]...)
		}
	}
	return nil
}
func (f *fakeStore) ListDNSZones() ([]store.DNSZone, error)            { return f.zones, nil }
func (f *fakeStore) ListDNSZoneAccess() ([]store.DNSZoneAccess, error) { return f.access, nil }
func (f *fakeStore) SetDNSZoneAccess(zoneID int, principals []string) error {
	kept := []store.DNSZoneAccess{}
	for _, a := range f.access {
		if a.ZoneID != zoneID {
			kept = append(kept, a)
		}
	}
	for _, p := range principals {
		kept = append(kept, store.DNSZoneAccess{ZoneID: zoneID, Principal: p})
	}
	f.access = kept
	return nil
}
func (f *fakeStore) GetDNSSubdomainLimit(scope string) (*store.DNSSubdomainLimit, error) {
	if v, ok := f.limits[scope]; ok {
		return &store.DNSSubdomainLimit{Scope: scope, Max: v}, nil
	}
	return nil, nil
}
func (f *fakeStore) CreateDNSAccount(a store.DNSAccount) (int, error) {
	f.nextID++
	a.ID = f.nextID
	f.accounts[a.ID] = &a
	return a.ID, nil
}
func (f *fakeStore) UpdateDNSAccount(a store.DNSAccount) error {
	f.accounts[a.ID] = &a
	return nil
}
func (f *fakeStore) DeleteDNSAccount(id int) error { delete(f.accounts, id); return nil }
func (f *fakeStore) GetDNSAccount(id int) (*store.DNSAccount, error) {
	if a, ok := f.accounts[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeStore) ListDNSAccounts() ([]store.DNSAccount, error) {
	out := []store.DNSAccount{}
	for _, a := range f.accounts {
		out = append(out, *a)
	}
	return out, nil
}
func (f *fakeStore) ListDNSOwnedRecords() ([]store.DNSOwnedRecord, error) { return f.owned, nil }
func (f *fakeStore) SetDNSAccountError(int, string) error                 { return nil }

type fakeProvider struct{ err error }

func (p *fakeProvider) ListZones(context.Context) ([]string, error) {
	return []string{"example.com"}, p.err
}
func (p *fakeProvider) GetRecords(context.Context, string) ([]dnsprov.Record, error) {
	return nil, p.err
}
func (p *fakeProvider) SetRecords(context.Context, string, []dnsprov.Record) ([]dnsprov.Record, error) {
	return nil, p.err
}
func (p *fakeProvider) DeleteRecords(context.Context, string, []dnsprov.Record) error { return p.err }

func newTestHandler(st *fakeStore, prov *fakeProvider) *DomainsHandler {
	h := NewDomainsHandler(Deps{Store: st, FeatureFlags: services.NewFeatureFlags(st), ClusterSecret: testSecret})
	h.newProvider = func(kind string, creds map[string]string) (dnsprov.Provider, error) {
		if _, err := dnsprov.New(kind, creds); err != nil {
			return nil, err
		}
		return prov, nil
	}
	return h
}

type caller struct {
	id    string
	admin bool
}

func call(h http.HandlerFunc, method, path, body string, who caller, vars map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), "userID", who.id)
	ctx = context.WithValue(ctx, "isAdmin", who.admin)
	req = mux.SetURLVars(req.WithContext(ctx), vars)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

var (
	admin  = caller{"admin", true}
	friend = caller{"friend", false}
	srv1   = map[string]string{"id": "1"}
)

// An invited member's domain counts against the server OWNER, not against the
// member: the member has a limit of zero of their own and still adds one.
func TestServerDomainsLimitCountsOwner(t *testing.T) {
	st := newFakeStore()
	zero := int64(0)
	st.limits["user:friend"] = &zero
	st.settings[services.DNSMaxPerUserSettingKey] = "2"
	st.domains = []store.ServerDomain{{ID: 1, ServerID: 2, FQDN: "old.example.org", Mode: store.DomainModeManual}}
	h := newTestHandler(st, &fakeProvider{})
	create := h.ServerGate(h.Create)

	rec := call(create, "POST", "/api/servers/1/domains", `{"mode":"manual","fqdn":"Play.Example.ORG."}`, friend, srv1)
	if rec.Code != http.StatusOK {
		t.Fatalf("first add: %d %s", rec.Code, rec.Body)
	}
	got := st.domains[len(st.domains)-1]
	if got.FQDN != "play.example.org" || got.State != services.DomainGrey || got.CreatedBy != "friend" || got.ServerID != 1 {
		t.Errorf("stored %+v, want play.example.org, grey, created by friend on server 1", got)
	}
	if len(st.serverRows) != 1 || st.serverRows[0].EventType != "server_domain.added" {
		t.Errorf("server audit = %+v, want one server_domain.added", st.serverRows)
	}

	// The owner now holds two (one on each server): the third is refused,
	// whoever asks.
	rec = call(create, "POST", "/api/servers/1/domains", `{"mode":"manual","fqdn":"third.example.org"}`, friend, srv1)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Subdomain limit reached (2)") {
		t.Fatalf("at the owner's limit: %d %s", rec.Code, rec.Body)
	}

	// The per-server limit is its own check.
	st.settings[services.DNSMaxPerUserSettingKey] = "unlimited"
	st.settings[services.DNSMaxPerServerSettingKey] = "1"
	rec = call(create, "POST", "/api/servers/1/domains", `{"mode":"manual","fqdn":"third.example.org"}`, friend, srv1)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "(1)") {
		t.Fatalf("at the server's limit: %d %s", rec.Code, rec.Body)
	}
}

func TestServerDomainsCreateRules(t *testing.T) {
	st := newFakeStore()
	st.roles["friend"] = 7
	h := newTestHandler(st, &fakeProvider{})
	create := h.ServerGate(h.Create)

	for _, tc := range []struct {
		name, body string
		who        caller
		want       int
	}{
		{"bad label", `{"mode":"managed","zoneId":5,"label":"-x"}`, admin, 400},
		{"wildcard", `{"mode":"managed","zoneId":5,"label":"*"}`, admin, 400},
		{"empty label", `{"mode":"managed","zoneId":5,"label":""}`, admin, 400},
		{"bad fqdn", `{"mode":"manual","fqdn":"https://x.com"}`, admin, 400},
		{"unknown mode", `{"mode":"magic","fqdn":"x.com"}`, admin, 400},
		{"unknown zone", `{"mode":"managed","zoneId":99,"label":"play"}`, admin, 400},
		{"no zone access", `{"mode":"managed","zoneId":5,"label":"play"}`, friend, 403},
	} {
		if rec := call(create, "POST", "/x", tc.body, tc.who, srv1); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	if len(st.domains) != 0 {
		t.Fatalf("a refused add stored %+v", st.domains)
	}

	// The role grant opens the zone; the label is lower-cased.
	st.access = []store.DNSZoneAccess{{ZoneID: 5, Principal: "role:7"}}
	if rec := call(create, "POST", "/x", `{"mode":"managed","zoneId":5,"label":"PLAY"}`, friend, srv1); rec.Code != 200 {
		t.Fatalf("role holder: %d %s", rec.Code, rec.Body)
	}
	d := st.domains[0]
	if d.FQDN != "play.example.com" || d.Label != "play" || d.ZoneID == nil || *d.ZoneID != 5 || d.Mode != store.DomainModeManaged {
		t.Errorf("stored %+v", d)
	}
	// Managed names are one per platform; manual ones once per server.
	if rec := call(create, "POST", "/x", `{"mode":"managed","zoneId":5,"label":"play"}`, admin, map[string]string{"id": "2"}); rec.Code != 409 {
		t.Errorf("managed duplicate on another server: %d", rec.Code)
	}
	if rec := call(create, "POST", "/x", `{"mode":"manual","fqdn":"play.example.com"}`, admin, srv1); rec.Code != 409 {
		t.Errorf("same name twice on one server: %d", rec.Code)
	}

	// Removing it from the wrong server is a 404; from its own, a removal.
	del := h.ServerGate(h.Delete)
	id := fmt.Sprint(d.ID)
	if rec := call(del, "DELETE", "/x", "", admin, map[string]string{"id": "2", "domainId": id}); rec.Code != 404 {
		t.Errorf("delete via another server: %d", rec.Code)
	}
	if rec := call(del, "DELETE", "/x", "", friend, map[string]string{"id": "1", "domainId": id}); rec.Code != 200 || len(st.domains) != 0 {
		t.Errorf("delete: %d, left %+v", rec.Code, st.domains)
	}
	if last := st.serverRows[len(st.serverRows)-1]; last.EventType != "server_domain.removed" {
		t.Errorf("last server audit = %s", last.EventType)
	}
}

// Losing zone access never removes a domain: it shows up flagged in the admin
// view and keeps listing on the server.
func TestServerDomainsListAndAccessLost(t *testing.T) {
	st := newFakeStore()
	st.users["friend"] = &models.User{ID: "friend", Username: "friend"}
	st.accounts[1] = &store.DNSAccount{ID: 1, Name: "cf", Provider: "cloudflare"}
	zone := 5
	st.domains = []store.ServerDomain{
		{ID: 1, ServerID: 1, FQDN: "play.example.com", Mode: store.DomainModeManaged, ZoneID: &zone, Label: "play", State: "green", CreatedBy: "friend"},
		{ID: 2, ServerID: 1, FQDN: "mc.other.org", Mode: store.DomainModeManual, State: "grey", CreatedBy: "friend"},
	}
	h := newTestHandler(st, &fakeProvider{})

	rec := call(h.ServerGate(h.List), "GET", "/x", "", friend, srv1)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Domains []struct {
			FQDN  string `json:"fqdn"`
			State string `json:"state"`
		} `json:"domains"`
		Zones         []zoneOption `json:"zones"`
		Limits        limitsView   `json:"limits"`
		DirectAddress string       `json:"directAddress"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Domains) != 2 || got.Domains[0].State != "green" || len(got.Zones) != 0 ||
		got.Limits.Used != 2 || got.Limits.Max != nil || got.DirectAddress != "203.0.113.10:25650" {
		t.Errorf("list = %+v", got)
	}

	rec = call(h.AdminGet, "GET", "/x", "", admin, nil)
	var adm struct {
		Accounts []struct {
			Zones []struct {
				Domains []struct {
					ID         int  `json:"id"`
					AccessLost bool `json:"accessLost"`
				} `json:"domains"`
			} `json:"zones"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &adm)
	hasID := false
	for _, a := range adm.Accounts {
		for _, z := range a.Zones {
			for _, d := range z.Domains {
				hasID = hasID || (d.ID > 0 && d.AccessLost)
			}
		}
	}
	if rec.Code != 200 || !hasID || !strings.Contains(rec.Body.String(), `"accessLost":true`) {
		t.Errorf("admin view: %d %s, want the domain flagged accessLost", rec.Code, rec.Body)
	}
}

// Ruling 2: only routing_mode "gateway" hides the feature; "both" is direct.
func TestGatewayModeHidesServerDomains(t *testing.T) {
	st := newFakeStore()
	h := newTestHandler(st, &fakeProvider{})
	for _, mode := range []string{"gateway", "both", "ip_port"} {
		st.settings["routing_mode"] = mode
		list := call(h.ServerGate(h.List), "GET", "/x", "", admin, srv1).Code
		create := call(h.ServerGate(h.Create), "POST", "/x", `{"mode":"manual","fqdn":"`+strings.ReplaceAll(mode, "_", "-")+`.example.org"}`, admin, srv1).Code
		want := 200
		if mode == "gateway" {
			want = 404
		}
		if list != want || create != want {
			t.Errorf("routing_mode %s: list %d, create %d, want %d", mode, list, create, want)
		}
	}

	st.settings["routing_mode"] = "ip_port"
	st.settings["feature_dns_updater_enabled"] = "false"
	if c := call(h.ServerGate(h.List), "GET", "/x", "", admin, srv1).Code; c != 404 {
		t.Errorf("flag off: server list %d, want 404", c)
	}
	if c := call(h.AdminGate(h.CreateAccount), "POST", "/x", `{}`, admin, nil).Code; c != 404 {
		t.Errorf("flag off: account create %d, want 404", c)
	}
	rec := call(h.AdminGet, "GET", "/x", "", admin, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("flag off: admin view %d %s, want 200 enabled:false", rec.Code, rec.Body)
	}
	if rec := call(h.SetSettings, "PUT", "/x", `{"enabled":true}`, admin, nil); rec.Code != 200 || st.settings["feature_dns_updater_enabled"] != "true" {
		t.Errorf("turning the flag on: %d %s", rec.Code, rec.Body)
	}
}

// The credential goes in and never comes out: not in a response, not in an
// audit row, not in a provider error the test endpoint passes on.
func TestAccountResponsesNeverCarryCredentials(t *testing.T) {
	st := newFakeStore()
	prov := &fakeProvider{}
	h := newTestHandler(st, prov)
	var bodies []string
	check := func(what string, rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("%s: %d %s, want %d", what, rec.Code, rec.Body, want)
		}
		bodies = append(bodies, rec.Body.String())
	}

	check("create", call(h.AdminGate(h.CreateAccount), "POST", "/x",
		`{"name":"Main","provider":"cloudflare","credentials":{"api_token":"`+testToken+`"}}`, admin, nil), 200)
	var id int
	for k := range st.accounts {
		id = k
	}
	vars := map[string]string{"id": fmt.Sprint(id)}
	check("get", call(h.AdminGet, "GET", "/x", "", admin, nil), 200)
	if !strings.Contains(bodies[1], `"hasCredentials":true`) {
		t.Errorf("admin view lacks hasCredentials: %s", bodies[1])
	}

	// A blank field keeps the stored credential.
	check("rename", call(h.AdminGate(h.UpdateAccount), "PUT", "/x",
		`{"name":"Renamed","provider":"cloudflare","credentials":{"api_token":""}}`, admin, vars), 200)
	creds, err := services.DecryptDNSCredentials(testSecret, st.accounts[id].EncCredentials)
	if err != nil || creds["api_token"] != testToken || st.accounts[id].Name != "Renamed" {
		t.Fatalf("after a blank-credential update: name %q creds ok=%v (%v)", st.accounts[id].Name, creds["api_token"] == testToken, err)
	}

	prov.err = errors.New("401 Unauthorized: token " + testToken + " is invalid")
	check("test", call(h.AdminGate(h.TestAccount), "POST", "/x", "", admin, vars), http.StatusBadGateway)
	if !strings.Contains(bodies[len(bodies)-1], "[redacted]") {
		t.Errorf("test error not scrubbed into the reply: %s", bodies[len(bodies)-1])
	}

	check("unknown field", call(h.AdminGate(h.CreateAccount), "POST", "/x",
		`{"name":"x","provider":"cloudflare","credentials":{"api_token":"a","password":"`+testToken+`"}}`, admin, nil), 400)
	check("missing field", call(h.AdminGate(h.CreateAccount), "POST", "/x",
		`{"name":"x","provider":"porkbun","credentials":{"api_key":"`+testToken+`"}}`, admin, nil), 400)

	prov.err = nil
	check("delete", call(h.AdminGate(h.DeleteAccount), "DELETE", "/x", "", admin, vars), 200)

	for i, b := range bodies {
		if strings.Contains(b, testToken) {
			t.Errorf("response %d carries the token: %s", i, b)
		}
	}
	if len(st.identity) != 3 {
		t.Errorf("identity audit rows = %d, want created, updated, deleted", len(st.identity))
	}
	for _, ev := range st.identity {
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), testToken) {
			t.Errorf("audit row carries the token: %s", raw)
		}
		want := 2
		if ev.EventType == "dns_account.deleted" {
			want = 3 // plus orphanedNames
		}
		if !strings.HasPrefix(ev.EventType, "dns_account.") || len(ev.Metadata) != want {
			t.Errorf("audit row %s metadata %v, want name and provider only (and orphanedNames on delete)", ev.EventType, ev.Metadata)
		}
	}
}

// When the provider cannot be reached, the account still goes and the admin is
// told which records are left behind - in the response and in the audit event,
// which outlives the page. The account's managed domains go with it, each
// audited on its server; other zones' and manual domains stay.
func TestDeleteAccountNamesOrphans(t *testing.T) {
	st := newFakeStore()
	enc, _ := services.EncryptDNSCredentials(testSecret, map[string]string{"api_token": testToken})
	st.accounts[1] = &store.DNSAccount{ID: 1, Name: "cf", Provider: "cloudflare", EncCredentials: enc}
	st.zones = append(st.zones, store.DNSZone{ID: 6, AccountID: 2, Name: "other.net", Listed: true})
	st.owned = []store.DNSOwnedRecord{{ID: 1, AccountID: 1, Zone: "example.com", Name: "play", Type: "A"}}
	five, six := 5, 6
	st.domains = []store.ServerDomain{
		{ID: 1, ServerID: 1, FQDN: "play.example.com", Mode: store.DomainModeManaged, ZoneID: &five, Label: "play"},
		{ID: 2, ServerID: 2, FQDN: "lobby.example.com", Mode: store.DomainModeManaged, ZoneID: &five, Label: "lobby"},
		{ID: 3, ServerID: 1, FQDN: "mc.other.net", Mode: store.DomainModeManaged, ZoneID: &six, Label: "mc"},
		{ID: 4, ServerID: 1, FQDN: "mc.customer.net", Mode: store.DomainModeManual},
	}
	h := newTestHandler(st, &fakeProvider{err: errors.New("network down")})
	rec := call(h.AdminGate(h.DeleteAccount), "DELETE", "/x", "", admin, map[string]string{"id": "1"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"orphanedNames":["play.example.com"]`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, ok := st.accounts[1]; ok {
		t.Error("account survived")
	}
	if len(st.identity) != 1 || !reflect.DeepEqual(st.identity[0].Metadata["orphanedNames"], []string{"play.example.com"}) {
		t.Errorf("identity audit = %+v, want orphanedNames in dns_account.deleted", st.identity)
	}
	left := []string{}
	for _, d := range st.domains {
		left = append(left, d.FQDN)
	}
	if !reflect.DeepEqual(left, []string{"mc.other.net", "mc.customer.net"}) {
		t.Errorf("domains left = %v, want the other zone's and the manual one", left)
	}
	removed := map[int]string{}
	for _, ev := range st.serverRows {
		if ev.EventType == "server_domain.removed" {
			removed[ev.ServerID] = ev.Metadata["fqdn"].(string)
		}
	}
	if !reflect.DeepEqual(removed, map[int]string{1: "play.example.com", 2: "lobby.example.com"}) {
		t.Errorf("server audit = %v, want one server_domain.removed per removed domain", removed)
	}
}

// Fix round 1: a provider change would leave the zones and the owned-record
// ledger on the account, and Core would then claim same-named records at the
// NEW provider as its own.
func TestUpdateAccountRefusesProviderChange(t *testing.T) {
	st := newFakeStore()
	enc, _ := services.EncryptDNSCredentials(testSecret, map[string]string{"api_token": testToken})
	st.accounts[1] = &store.DNSAccount{ID: 1, Name: "cf", Provider: "cloudflare", EncCredentials: enc}
	h := newTestHandler(st, &fakeProvider{})
	rec := call(h.AdminGate(h.UpdateAccount), "PUT", "/x",
		`{"name":"cf","provider":"hetzner","credentials":{"api_token":"other"}}`, admin, map[string]string{"id": "1"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "cannot be changed") {
		t.Fatalf("provider change: %d %s, want 409", rec.Code, rec.Body)
	}
	if a := st.accounts[1]; a.Provider != "cloudflare" || a.EncCredentials != enc {
		t.Errorf("account changed anyway: %+v", a)
	}
}

// A routing mode that cannot be read must not fail open into direct mode.
func TestServerGateFailsClosedOnSettingError(t *testing.T) {
	st := newFakeStore()
	h := newTestHandler(st, &fakeProvider{})
	gate := h.ServerGate(h.List)
	st.settings["feature_dns_updater_enabled"] = "true"
	flagOnly := &settingErrStore{fakeStore: st, failKey: "routing_mode"}
	h.d.Store = flagOnly
	if rec := call(gate, "GET", "/x", "", admin, srv1); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("routing_mode unreadable: %d %s, want 503", rec.Code, rec.Body)
	}
	// Never saved (sql.ErrNoRows) is the ip_port default, i.e. direct.
	delete(st.settings, "routing_mode")
	h.d.Store = st
	if rec := call(gate, "GET", "/x", "", admin, srv1); rec.Code != 200 {
		t.Fatalf("routing_mode unset: %d %s, want 200", rec.Code, rec.Body)
	}
}

type settingErrStore struct {
	*fakeStore
	failKey string
}

func (s *settingErrStore) GetSetting(k string) (string, error) {
	if k == s.failKey {
		return "", errors.New("connection reset")
	}
	return s.fakeStore.GetSetting(k)
}

func TestZoneForbiddenDoesNotNameTheZone(t *testing.T) {
	st := newFakeStore()
	h := newTestHandler(st, &fakeProvider{})
	rec := call(h.ServerGate(h.Create), "POST", "/x", `{"mode":"managed","zoneId":5,"label":"play"}`, friend, srv1)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "example.com") {
		t.Fatalf("forbidden zone: %d %s, want 403 without the zone name", rec.Code, rec.Body)
	}
}

func TestSetSettingsIsAudited(t *testing.T) {
	st := newFakeStore()
	h := newTestHandler(st, &fakeProvider{})
	rec := call(h.SetSettings, "PUT", "/x", `{"intervalSeconds":10,"perUser":"3"}`, admin, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(st.identity) != 1 || st.identity[0].EventType != "dns_settings.updated" ||
		st.identity[0].Metadata[services.DNSIntervalSettingKey] != "30" ||
		st.identity[0].Metadata[services.DNSMaxPerUserSettingKey] != "3" || len(st.identity[0].Metadata) != 2 {
		t.Errorf("audit = %+v", st.identity)
	}
	if call(h.SetSettings, "PUT", "/x", `{}`, admin, nil); len(st.identity) != 1 {
		t.Error("an empty save wrote an audit row")
	}
}

// A zone the provider no longer lists keeps its domains but takes no new one,
// and the list says how old a state may be.
func TestUnlistedZoneTakesNoNewDomain(t *testing.T) {
	st := newFakeStore()
	st.zones[0].Listed = false
	st.accounts[1] = &store.DNSAccount{ID: 1, Name: "cf", Provider: "cloudflare"}
	st.settings[services.DNSIntervalSettingKey] = "120"
	h := newTestHandler(st, &fakeProvider{})

	rec := call(h.ServerGate(h.Create), "POST", "/api/servers/1/domains", `{"mode":"managed","zoneId":5,"label":"play"}`, admin, srv1)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no longer lists that zone") {
		t.Fatalf("create in an unlisted zone: %d %s", rec.Code, rec.Body)
	}
	rec = call(h.ServerGate(h.List), "GET", "/api/servers/1/domains", "", admin, srv1)
	var body struct {
		Zones           []zoneOption `json:"zones"`
		IntervalSeconds int          `json:"intervalSeconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Zones) != 0 || body.IntervalSeconds != 120 {
		t.Fatalf("list = %s, want no zone to pick and intervalSeconds 120", rec.Body)
	}
	rec = call(h.AdminGet, "GET", "/api/admin/dns", "", admin, nil)
	if !strings.Contains(rec.Body.String(), `"listed":false`) {
		t.Fatalf("admin view does not say the zone is unlisted: %s", rec.Body)
	}
}
