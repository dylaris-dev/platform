package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// remoteReadsStore adds the remote-gateway tables to proxyProtoFakeStore.
type remoteReadsStore struct {
	*proxyProtoFakeStore
	kits   map[uint]store.RemoteKit
	routes map[string]store.RemoteRoute
	outbox []struct{ op, domain string }
	limits map[string]*int // scope -> max routes; a missing scope has no row
}

func (f *remoteReadsStore) GetGatewayRouteLimit(scope string) (*models.GatewayRouteLimit, error) {
	if m, ok := f.limits[scope]; ok {
		return &models.GatewayRouteLimit{Scope: scope, MaxRoutes: m}, nil
	}
	return nil, nil
}
func (f *remoteReadsStore) ListServersByOwner(owner string) ([]models.Server, error) {
	var out []models.Server
	for _, s := range f.servers {
		if s.OwnerID == owner {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *remoteReadsStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Name: "node"}, nil
}
func (f *remoteReadsStore) GetRemoteKit(nodeID uint) (*store.RemoteKit, error) {
	if k, ok := f.kits[nodeID]; ok {
		return &k, nil
	}
	return nil, nil
}
func (f *remoteReadsStore) InsertRemoteKit(k store.RemoteKit) (bool, error) {
	if _, ok := f.kits[k.NodeID]; ok {
		return false, nil
	}
	f.kits[k.NodeID] = k
	return true, nil
}
func (f *remoteReadsStore) ListRemoteKits() ([]store.RemoteKit, error) {
	var out []store.RemoteKit
	for _, k := range f.kits {
		out = append(out, k)
	}
	return out, nil
}
func (f *remoteReadsStore) UpsertRemoteRoute(r store.RemoteRoute) error {
	f.routes[r.Domain] = r
	return nil
}
func (f *remoteReadsStore) ClaimRemoteRoute(r store.RemoteRoute) (bool, *store.RemoteRoute, error) {
	if held, ok := f.routes[r.Domain]; ok {
		return false, &held, nil
	}
	f.routes[r.Domain] = r
	return true, nil, nil
}
func (f *remoteReadsStore) GetRemoteRoute(domain string) (*store.RemoteRoute, error) {
	if r, ok := f.routes[domain]; ok {
		return &r, nil
	}
	return nil, nil
}
func (f *remoteReadsStore) DeleteRemoteRoute(domain string) error {
	delete(f.routes, domain)
	return nil
}
func (f *remoteReadsStore) ListRemoteRoutesByServer(uuid string) ([]store.RemoteRoute, error) {
	var out []store.RemoteRoute
	for _, r := range f.routes {
		if r.ServerUUID == uuid {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *remoteReadsStore) HasOpenOutbox(domain, op string) (bool, error) {
	for _, it := range f.outbox {
		if it.domain == domain && (op == "" || it.op == op) {
			return true, nil
		}
	}
	return false, nil
}
func (f *remoteReadsStore) EnqueueOutbox(op, domain string, _ any) error {
	f.outbox = append(f.outbox, struct{ op, domain string }{op, domain})
	return nil
}
func (f *remoteReadsStore) DeleteRouteEdition(string) error { return nil }

const remoteSrvUUID = "0b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f"

// newRemoteGatewayHandler is a handler on a platform whose gateway is the
// managed one. The local Redis holds a route of the same server, which no
// remote answer may show.
func newRemoteGatewayHandler(t *testing.T) (*GatewayHandler, *remoteReadsStore, *remotegw.Fake) {
	t.Helper()
	h, base, rdb := newProxyProtoHandler(t, true)
	base.servers[7].UUID = remoteSrvUUID
	base.servers[7].NodeID = 3
	fs := &remoteReadsStore{proxyProtoFakeStore: base, kits: map[uint]store.RemoteKit{}, routes: map[string]store.RemoteRoute{}, limits: map[string]*int{}}
	seedRoute(t, rdb, "local.example.com", "tok-a", capsClientAddr)
	fake := remotegw.NewFake(t)
	remote := func() (bool, bool) { return true, true }
	rg := services.NewRemoteGateway(fs, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	h.d.Store = fs
	h.d.GatewayRemote = remote
	h.d.RemoteGateway = rg
	h.d.Gateway = services.NewSwitchingGateway(nil, rg, remote)
	return h, fs, fake
}

func serveVars(h http.HandlerFunc, method, target, body string, vars map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r = mux.SetURLVars(r, vars)
	r = r.WithContext(context.WithValue(r.Context(), "userID", "owner-1"))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func TestRemoteServerRoutesComeFromLocalTable(t *testing.T) {
	h, fs, fake := newRemoteGatewayHandler(t)
	fs.routes["a.example.com"] = store.RemoteRoute{Domain: "a.example.com", ServerUUID: remoteSrvUUID, NodeID: 3, Port: 25570, ProxyProtocol: true}
	fs.routes["gone.example.com"] = store.RemoteRoute{Domain: "gone.example.com", ServerUUID: remoteSrvUUID, NodeID: 3, Port: 25565}
	fs.routes["other.example.com"] = store.RemoteRoute{Domain: "other.example.com", ServerUUID: "someone-else", NodeID: 3, Port: 25565}
	fs.outbox = append(fs.outbox, struct{ op, domain string }{services.OutboxOpDelete, "gone.example.com"})
	vars := map[string]string{"id": "7"}

	rec := serveVars(h.GetServerRoutes, http.MethodGet, "/api/servers/7/routes", "", vars)
	var got []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(got) != 1 || got[0]["domain"] != "a.example.com" || got[0]["target_port"] != float64(25570) ||
		got[0]["proxy_protocol"] != true || got[0]["server_uuid"] != remoteSrvUUID || got[0]["target_ip"] != "mc-"+remoteSrvUUID {
		t.Fatalf("routes = %v, want only a.example.com from the local table", got)
	}
	if _, leaked := got[0]["tunnel_id"]; leaked {
		t.Fatal("a tunnel id in a route listing")
	}

	// Delete: only an address of this server, queued for the provider.
	vars["domain"] = "other.example.com"
	if rec := serveVars(h.DeleteServerRoute, http.MethodDelete, "/", "", vars); rec.Code != http.StatusNotFound {
		t.Fatalf("another server's route: %d, want 404", rec.Code)
	}
	vars["domain"] = "local.example.com"
	if rec := serveVars(h.DeleteServerRoute, http.MethodDelete, "/", "", vars); rec.Code != http.StatusNotFound {
		t.Fatalf("a route only the local Redis knows: %d, want 404", rec.Code)
	}
	vars["domain"] = "a.example.com"
	if rec := serveVars(h.DeleteServerRoute, http.MethodDelete, "/", "", vars); rec.Code != http.StatusOK {
		t.Fatalf("own route: %d %s", rec.Code, rec.Body)
	}
	if open, _ := fs.HasOpenOutbox("a.example.com", services.OutboxOpDelete); !open {
		t.Fatal("delete not queued for the provider")
	}
	if c := fake.Calls(); len(c) != 0 {
		t.Fatalf("provider calls %v, want none (the outbox sends it)", c)
	}
}

func TestRemoteCheckDomain(t *testing.T) {
	h, _, fake := newRemoteGatewayHandler(t)
	check := func(q string) (int, map[string]interface{}) {
		rec := serveVars(h.CheckDomainAvailability, http.MethodGet, "/api/gateway/check-domain?"+q, "", nil)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// local.example.com is in the local Redis; the provider is asked, not it.
	if code, out := check("domain=Local.Example.com"); code != 200 || out["available"] != true || out["domain"] != "local.example.com" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := check("subdomain=survival&hosterDomain=example.com"); code != 200 || out["available"] != true || out["domain"] != "survival.example.com" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := check("customDomain=play.acme.tld"); code != 200 || out["available"] != false || out["reason"] != "custom domains are not enabled" {
		t.Fatalf("custom domain: %d %v", code, out)
	}
	if c := fake.Calls(); len(c) != 3 || c[0] != "GET /regions" {
		t.Fatalf("provider calls = %v, want the hoster list once and two checks", c)
	}
	if code, out := check("domain=play.foreign.tld"); code != 200 || out["available"] != false || out["reason"] != "custom domains are not enabled" {
		t.Fatalf("foreign domain: %d %v", code, out)
	}
	fake.FailNext(1, 401, "", 0)
	if code, out := check("domain=x.example.com"); code != http.StatusBadGateway || out["code"] != "provider_key_rejected" {
		t.Fatalf("rejected key: %d %v", code, out)
	}
}

func TestRemoteCreateServerRouteMapsProviderRefusals(t *testing.T) {
	h, fs, fake := newRemoteGatewayHandler(t)
	create := func(body string) *httptest.ResponseRecorder {
		return serveVars(h.CreateServerRoute, http.MethodPost, "/", body, map[string]string{"id": "7"})
	}
	if rec := create(`{"subdomain":"survival","hosterDomain":"example.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if r, ok := fs.routes["survival.example.com"]; !ok || r.NodeID != 3 || r.Port != 25565 {
		t.Fatalf("local row = %+v", fs.routes)
	}
	fake.FailNext(1, 409, "domain_taken", 0)
	rec := create(`{"domain":"taken.example.com"}`)
	if rec.Code != http.StatusConflict || decodeCode(t, rec) != "domain_taken" {
		t.Fatalf("taken: %d %s", rec.Code, rec.Body)
	}
}

// The customer side's own rules still hold on the managed gateway, and a
// refused request never reaches the provider's route endpoint.
func TestRemoteCreateServerRouteKeepsLocalRules(t *testing.T) {
	ptr := func(n int) *int { return &n }
	cases := []struct {
		name   string
		body   string
		limit  *int // user_default; nil = no row
		held   int
		status int
		want   string
	}{
		{"foreign raw domain", `{"domain":"play.foreign.tld"}`, nil, 0, 400, "custom domains are not enabled"},
		{"no dot boundary", `{"domain":"evilexample.com"}`, nil, 0, 400, "custom domains are not enabled"},
		{"custom domain", `{"customDomain":"play.acme.tld"}`, nil, 0, 400, "custom domains are not enabled"},
		{"unknown hoster", `{"subdomain":"survival","hosterDomain":"other.tld"}`, nil, 0, 400, "hoster domain not configured"},
		{"reserved prefix", `{"subdomain":"admin","hosterDomain":"example.com"}`, nil, 0, 400, "reserved"},
		{"reserved prefix, raw", `{"domain":"admin.example.com"}`, nil, 0, 400, "reserved"},
		{"subdomain format", `{"subdomain":"a_b","hosterDomain":"example.com"}`, nil, 0, 400, "allowed format"},
		{"routes disabled", `{"subdomain":"survival","hosterDomain":"example.com"}`, ptr(0), 0, 403, "disabled for your account"},
		{"cap reached", `{"subdomain":"survival","hosterDomain":"example.com"}`, ptr(1), 1, 403, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, fs, fake := newRemoteGatewayHandler(t)
			if tc.limit != nil {
				fs.limits["user_default"] = tc.limit
			}
			for i := 0; i < tc.held; i++ {
				d := fmt.Sprintf("held%d.example.com", i)
				fs.routes[d] = store.RemoteRoute{Domain: d, ServerUUID: remoteSrvUUID, NodeID: 3}
			}
			rec := serveVars(h.CreateServerRoute, http.MethodPost, "/", tc.body, map[string]string{"id": "7"})
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%d %s, want %d %q", rec.Code, rec.Body, tc.status, tc.want)
			}
			for _, c := range fake.Calls() {
				if c != "GET /regions" {
					t.Fatalf("provider calls %v, want only the hoster list", fake.Calls())
				}
			}
		})
	}

	// A pending delete does not count against the cap.
	h, fs, _ := newRemoteGatewayHandler(t)
	one := 1
	fs.limits["user_default"] = &one
	fs.routes["old.example.com"] = store.RemoteRoute{Domain: "old.example.com", ServerUUID: remoteSrvUUID, NodeID: 3}
	fs.outbox = append(fs.outbox, struct{ op, domain string }{services.OutboxOpDelete, "old.example.com"})
	if rec := serveVars(h.CreateServerRoute, http.MethodPost, "/", `{"subdomain":"survival","hosterDomain":"example.com"}`, map[string]string{"id": "7"}); rec.Code != http.StatusCreated {
		t.Fatalf("with a pending delete: %d %s", rec.Code, rec.Body)
	}

	// The provider's hoster list cannot be read: refuse, never pass through.
	h, _, fake := newRemoteGatewayHandler(t)
	fake.FailNext(1, 500, "internal", 0)
	rec := serveVars(h.CreateServerRoute, http.MethodPost, "/", `{"domain":"survival.example.com"}`, map[string]string{"id": "7"})
	if rec.Code != http.StatusBadGateway || len(fake.Calls()) != 1 {
		t.Fatalf("regions down: %d %s, calls %v", rec.Code, rec.Body, fake.Calls())
	}
}

func TestRemoteProxyProtocolUsesKitCaps(t *testing.T) {
	h, fs, fake := newRemoteGatewayHandler(t)
	patch := func(body string) *httptest.ResponseRecorder {
		return serveVars(h.SetServerProxyProtocol, http.MethodPatch, "/", body, map[string]string{"id": "7"})
	}
	code := func(rec *httptest.ResponseRecorder) string {
		var out struct{ Code string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Code
	}

	// The local Redis has a route with client-addr; it does not count.
	if rec := patch(`{"mode":"on"}`); rec.Code != http.StatusConflict || code(rec) != "no_routes" {
		t.Fatalf("no remote routes: %d %s", rec.Code, rec.Body)
	}
	fs.routes["a.example.com"] = store.RemoteRoute{Domain: "a.example.com", ServerUUID: remoteSrvUUID, NodeID: 3, Port: 25565}
	if rec := patch(`{"mode":"on"}`); rec.Code != http.StatusConflict || code(rec) != "link_without_client_addr" {
		t.Fatalf("node without a kit: %d %s", rec.Code, rec.Body)
	}
	minted, err := remotegw.New(fake.URL(), fake.Key).MintKit(context.Background(), "node", "")
	if err != nil {
		t.Fatal(err)
	}
	fs.kits[3] = store.RemoteKit{NodeID: 3, LinkID: minted.LinkID}
	if rec := patch(`{"mode":"on"}`); rec.Code != http.StatusConflict || code(rec) != "link_without_client_addr" {
		t.Fatalf("offline kit: %d %s", rec.Code, rec.Body)
	}
	if fs.servers[7].ProxyProtocol {
		t.Fatal("a refused switch was stored")
	}

	fake.SetKitOnline(minted.LinkID, "client-addr")
	// A fresh RemoteGateway: the kit list is cached for 30 s.
	rg := services.NewRemoteGateway(fs, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	h.d.RemoteGateway, h.d.Gateway = rg, services.NewSwitchingGateway(nil, rg, h.d.GatewayRemote)
	rec := patch(`{"mode":"on"}`)
	if rec.Code != http.StatusOK || !fs.servers[7].ProxyProtocol {
		t.Fatalf("kit with client-addr: %d %s", rec.Code, rec.Body)
	}
	if open, _ := fs.HasOpenOutbox("a.example.com", services.OutboxOpProxy); !open {
		t.Fatal("the switch was not queued for the provider")
	}

	// Auto never acts on the managed gateway, so it is refused and reported off.
	if rec := patch(`{"mode":"auto"}`); rec.Code != http.StatusConflict || code(rec) != "gateway_remote" {
		t.Fatalf("auto: %d %s", rec.Code, rec.Body)
	}
	rec = serveVars(h.GetServerProxyProtocol, http.MethodGet, "/", "", map[string]string{"id": "7"})
	var view map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view["autoAvailable"] != false {
		t.Fatalf("GET view = %s", rec.Body)
	}
}

// Local mode gets none of the remote fields or branches.
func TestLocalProxyProtocolViewUnchanged(t *testing.T) {
	h, _, _ := newProxyProtoHandler(t, true)
	rec := serveVars(h.GetServerProxyProtocol, http.MethodGet, "/", "", map[string]string{"id": "7"})
	var view map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if _, ok := view["autoAvailable"]; ok {
		t.Fatalf("local view carries autoAvailable: %s", rec.Body)
	}
}

func TestRemoteReadsAnswer503WhileTheModeIsUnknown(t *testing.T) {
	h, _, _ := newRemoteGatewayHandler(t)
	h.d.GatewayRemote = func() (bool, bool) { return false, false }
	for name, fn := range map[string]http.HandlerFunc{
		"routes": h.GetServerRoutes, "check-domain": h.CheckDomainAvailability, "delete": h.DeleteServerRoute,
		"proxy-protocol view": h.GetServerProxyProtocol,
	} {
		rec := serveVars(fn, http.MethodGet, "/?domain=a.example.com", "", map[string]string{"id": "7", "domain": "a.example.com"})
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", name, rec.Code)
		}
	}
	// Off is never refused: the column is written whatever the mode.
	h.d.Store.(*remoteReadsStore).servers[7].ProxyProtocol = true
	rec := serveVars(h.SetServerProxyProtocol, http.MethodPatch, "/", `{"mode":"off"}`, map[string]string{"id": "7"})
	if rec.Code == http.StatusServiceUnavailable || h.d.Store.(*remoteReadsStore).servers[7].ProxyProtocol {
		t.Fatalf("off while the mode is unknown: %d %s", rec.Code, rec.Body)
	}
	if rec := serveVars(h.SetServerProxyProtocol, http.MethodPatch, "/", `{"mode":"on"}`, map[string]string{"id": "7"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("on while the mode is unknown: %d, want 503", rec.Code)
	}
}

// Another server of this platform holds the address: the provider sees one
// account and would take the create as an edit, so the refusal is local, and
// check-domain says taken without asking the provider.
func TestRemoteAddressOfAnotherServerIsTaken(t *testing.T) {
	h, fs, fake := newRemoteGatewayHandler(t)
	held := store.RemoteRoute{Domain: "arena.example.com", ServerUUID: "1c7a2d3f-7b4e-4d2c-8f80-2b3c4d5e6f70", NodeID: 4, Port: 25570}
	fs.routes[held.Domain] = held

	rec := serveVars(h.CreateServerRoute, http.MethodPost, "/", `{"subdomain":"arena","hosterDomain":"example.com"}`, map[string]string{"id": "7"})
	if rec.Code != http.StatusConflict || decodeCode(t, rec) != "domain_taken" {
		t.Fatalf("create: %d %s, want 409 domain_taken", rec.Code, rec.Body)
	}
	if fs.routes[held.Domain] != held {
		t.Fatalf("row = %+v, want it unchanged", fs.routes[held.Domain])
	}
	rec = serveVars(h.CheckDomainAvailability, http.MethodGet, "/api/gateway/check-domain?subdomain=arena&hosterDomain=example.com", "", nil)
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out["available"] != false || out["reason"] != nil {
		t.Fatalf("check-domain: %d %s, want taken with no reason", rec.Code, rec.Body)
	}
	for _, c := range fake.Calls() {
		if c != "GET /regions" {
			t.Fatalf("provider calls %v, want only the hoster list", fake.Calls())
		}
	}
}

// Provider reason codes reach the user as the local path's sentences.
func TestRemoteCheckReasonSentences(t *testing.T) {
	for code, want := range map[string]string{
		"taken":      "",
		"blocked":    `subdomain "admin" is reserved`,
		"not_hosted": "custom domains are not enabled",
		"invalid":    "invalid domain format",
		"new_code":   "",
	} {
		if got := remoteCheckReason(code, "admin.example.com"); got != want {
			t.Errorf("%s: %q, want %q", code, got, want)
		}
	}
}
