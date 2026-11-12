package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// Two real tenants. Everything of tenant A that tenant B never sends itself
// is a marker: none of it may appear in any answer B gets.
const (
	extTenantA    = "user-a"
	extTenantB    = "user-b"
	extTenantC    = "user-c" // holds nothing at all
	extTargetA    = "10.66.66.66"
	extPortA      = 31337
	extTXTA       = "txt-secret-of-tenant-a"
	extKitNameA   = "tenant-a-kit"
	extUsageBytes = 777777
)

var extMarkersOfA = []string{extTenantA, extTargetA, "31337", extTXTA, extKitNameA, "777777"}

// extStore is a multi-tenant fake: every read is filtered by owner the way the
// real queries are, so a handler that forgot to scope would see the other
// tenant's rows and the test would catch it.
type extStore struct {
	store.Store

	settings map[string]string
	kits     []store.WarpAPIKey
	routes   []store.CoreLinkRoute
	claims   []store.CustomDomainClaim
	regions  map[string][]store.RegionUsage
	usage    map[string]*store.TrafficUsage  // by user id, any period
	limits   map[string]*models.TrafficLimit // by scope ("user:<id>")
}

func (f *extStore) GetTrafficUsage(userID string, period time.Time) (*store.TrafficUsage, error) {
	if u := f.usage[userID]; u != nil {
		return u, nil
	}
	return &store.TrafficUsage{UserID: userID, Period: period}, nil
}

func (f *extStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if region != "*" || kind != "edge" {
		return nil, nil
	}
	return f.limits[scope], nil
}

func (f *extStore) GetSetting(key string) (string, error) { return f.settings[key], nil }

func (f *extStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	five := int64(5)
	return &store.UserBilling{UserID: userID, Status: "active", MaxLinks: &five}, nil
}

func (f *extStore) GetGatewayCustomDomainLimit(string) (*models.GatewayCustomDomainLimit, error) {
	return nil, errors.New("not found")
}

func (f *extStore) GetGatewayRouteLimit(string) (*models.GatewayRouteLimit, error) {
	return nil, errors.New("not found")
}

func (f *extStore) ListServersByOwner(string) ([]models.Server, error) { return nil, nil }
func (f *extStore) ListLinkRouteVoice() ([]store.LinkRouteVoice, error) {
	return nil, nil
}
func (f *extStore) GetVoicePermission(string) (*bool, error) { return nil, nil }
func (f *extStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *extStore) ListLinkRouteEditions() ([]store.LinkRouteEdition, error) { return nil, nil }

func (f *extStore) ListWarpAPIKeysByOwner(owner string) ([]store.WarpAPIKey, error) {
	var out []store.WarpAPIKey
	for _, k := range f.kits {
		if k.OwnerID == owner {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *extStore) CountLinkKitsByOwner(owner string) (int, error) {
	kits, _ := f.ListWarpAPIKeysByOwner(owner)
	return len(kits), nil
}

func (f *extStore) GetWarpAPIKeyByNodeID(nodeID string) (*store.WarpAPIKey, error) {
	for i := range f.kits {
		if f.kits[i].NodeID == nodeID {
			k := f.kits[i]
			return &k, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *extStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) { return f.routes, nil }

func (f *extStore) claim(userID, domain string) int {
	for i, c := range f.claims {
		if c.UserID == userID && c.Domain == domain {
			return i
		}
	}
	return -1
}

func (f *extStore) GetCustomDomainClaim(userID, domain string) (*store.CustomDomainClaim, error) {
	if i := f.claim(userID, domain); i >= 0 {
		c := f.claims[i]
		return &c, nil
	}
	return nil, store.ErrNoClaim
}

func (f *extStore) RequestCustomDomainClaim(userID, domain string) (*store.CustomDomainClaim, error) {
	if i := f.claim(userID, domain); i >= 0 {
		c := f.claims[i]
		return &c, nil
	}
	c := store.CustomDomainClaim{ID: 100 + len(f.claims), UserID: userID, Domain: domain, State: store.ClaimPending}
	f.claims = append(f.claims, c)
	return &c, nil
}

func (f *extStore) SetCustomDomainTXTToken(id int, token string) error {
	for i := range f.claims {
		if f.claims[i].ID == id {
			f.claims[i].TXTToken = token
		}
	}
	return nil
}

func (f *extStore) ListCustomDomainClaimsByUser(userID string) ([]store.CustomDomainClaim, error) {
	var out []store.CustomDomainClaim
	for _, c := range f.claims {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *extStore) DeleteCustomDomainClaim(userID, domain string) (bool, error) {
	i := f.claim(userID, domain)
	if i < 0 {
		return false, nil
	}
	f.claims = append(f.claims[:i], f.claims[i+1:]...)
	return true, nil
}

func (f *extStore) GetTrafficUsageRegions(userID string, _ time.Time) ([]store.RegionUsage, error) {
	return f.regions[userID], nil
}

// extGateway refuses a domain somebody else holds, like the real hub bridge,
// and records every write so a test can prove nothing of A's was touched.
type extGateway struct {
	*linkRouteFakeGateway
	taken   map[string]string
	deleted []string
	proxied []string
}

func (g *extGateway) CreateRouteViaLink(owner, domain, token, host string, port int) error {
	if o, ok := g.taken[domain]; ok && o != owner {
		return services.ErrRouteDomainTaken
	}
	return g.linkRouteFakeGateway.CreateRouteViaLink(owner, domain, token, host, port)
}

func (g *extGateway) DeleteCoreOwnedRoute(domain string) error {
	g.deleted = append(g.deleted, domain)
	return nil
}

func (g *extGateway) SetCoreOwnedRouteProxyProtocol(domain string, _ bool) error {
	g.proxied = append(g.proxied, domain)
	return nil
}

func passGate(next http.HandlerFunc) http.HandlerFunc { return next }

type extFixture struct {
	h   *ExternalGatewayHandler
	st  *extStore
	gw  *extGateway
	rdb *redis.Client
}

func newExtFixture(t *testing.T) *extFixture {
	t.Helper()
	st := &extStore{
		settings: map[string]string{
			services.HosterDomainsSettingKey: `[{"domain":"example.com","validation":"dns"}]`,
			"routing_mode":                   "gateway",
			"gateway_custom_domains_enabled": "true",
		},
		kits: []store.WarpAPIKey{
			{ID: 1, Name: extKitNameA, NodeID: "link-a", OwnerID: extTenantA},
			{ID: 2, Name: "tenant-b-kit", NodeID: "link-b", OwnerID: extTenantB},
		},
		routes: []store.CoreLinkRoute{
			{Domain: "a.example.com", OwnerID: extTenantA, LinkToken: "derived-token-link-a", TargetHost: extTargetA, TargetPort: extPortA},
			{Domain: "b.example.com", OwnerID: extTenantB, LinkToken: "derived-token-link-b", TargetHost: "192.168.1.20", TargetPort: 25565},
		},
		claims: []store.CustomDomainClaim{
			{ID: 1, UserID: extTenantA, Domain: "a-own.net", State: store.ClaimPending, TXTToken: extTXTA},
			{ID: 2, UserID: extTenantB, Domain: "b-own.net", State: store.ClaimVerified, TXTToken: "txt-of-b"},
		},
		regions: map[string][]store.RegionUsage{
			extTenantA: {{Region: "eu", Kind: "edge", Product: "routeonly", Bytes: extUsageBytes}},
			extTenantB: {{Region: "eu", Kind: "edge", Product: "routeonly", Bytes: 1000}},
		},
	}
	gw := &extGateway{linkRouteFakeGateway: &linkRouteFakeGateway{}, taken: map[string]string{
		"a.example.com": extTenantA, "b.example.com": extTenantB,
	}}
	rdb := newLinkRouteRedis(t)
	seedLinkRoute(t, rdb, "a.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: extTenantA,
		TunnelID: "derived-token-link-a", TargetIP: extTargetA, TargetPort: extPortA})
	seedLinkRoute(t, rdb, "b.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: extTenantB,
		TunnelID: "derived-token-link-b", TargetIP: "192.168.1.20", TargetPort: 25565})

	d := Deps{Store: st, Gateway: gw, Redis: rdb, FeatureFlags: services.NewFeatureFlags(routeOnlyOnFlags{})}
	gh := NewGatewayHandler(d)
	gh.txtResolver = noTXT{}
	usage := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "usage": map[string]interface{}{"edgeBytes": 42}})
	}
	routeOptions := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
			"hosterDomains": []services.HosterDomain{{Domain: "example.com", Validation: "dns"}}, "customDomainsEnabled": true, "cnameTarget": "route"})
	}
	h := NewExternalGatewayHandler(NewWarpHandler(d, nil), gh, NewCustomDomainHandler(d, noTXT{}), passGate, routeOptions, usage)
	return &extFixture{h: h, st: st, gw: gw, rdb: rdb}
}

// extReq builds the request the way it arrives behind APIKeyOwnerRoute +
// ExternalOwnerRoute: a key in the context and the KEY OWNER's identity,
// including the owner's admin flag.
func extReq(method, target, owner string, admin bool, vars map[string]string, body interface{}) *http.Request {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, target, rd)
	ctx := context.WithValue(r.Context(), "userID", owner)
	ctx = context.WithValue(ctx, "username", owner)
	ctx = context.WithValue(ctx, "isAdmin", admin)
	ctx = reqctx.WithAPIKey(ctx, &models.APIKey{ID: 7, UserID: owner}, false)
	r = r.WithContext(ctx)
	if vars != nil {
		r = mux.SetURLVars(r, vars)
	}
	return r
}

func decodeExt(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json; body %s", ct, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, rec.Body.String())
	}
	return out
}

// An admin's key must get exactly what a tenant gets: the session handler
// skips TXT proof for an admin, the external route must not.
func TestExternalGatewayForcesNonAdmin(t *testing.T) {
	body := map[string]interface{}{"linkId": "link-b", "customDomain": "admin-own.net", "targetHost": "192.168.1.5"}

	// The premise: the session handler, given an admin context, creates it.
	f := newExtFixture(t)
	rec := httptest.NewRecorder()
	f.h.gw.CreateLinkRoute(rec, extReq("POST", "/", extTenantB, true, nil, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("premise: session handler as admin = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	f = newExtFixture(t)
	rec = httptest.NewRecorder()
	f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, true, nil, body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("external as admin = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if got := decodeExt(t, rec)["code"]; got != "domain_unverified" {
		t.Fatalf("code = %v, want domain_unverified", got)
	}
	if n := len(f.gw.createRouteViaLinkCalls); n != 0 {
		t.Fatalf("gateway got %d create calls, want 0", n)
	}

	// And the handler behind it really sees isAdmin=false.
	var seen interface{}
	externalGateway(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Context().Value("isAdmin")
		w.Write([]byte(`{"success":true}`))
	})(httptest.NewRecorder(), extReq("GET", "/", extTenantB, true, nil, nil))
	if seen != false {
		t.Fatalf("inner handler saw isAdmin = %v, want false", seen)
	}
}

// Tenant B on every endpoint, aimed at tenant A's link, route and custom
// domain. Run once with B as a plain tenant and once with B's key owned by an
// admin: the second is what proves the admin bypasses are switched off.
func TestExternalGatewayTenantIsolation(t *testing.T) {
	type call func(h *ExternalGatewayHandler, w http.ResponseWriter, r *http.Request)
	cases := []struct {
		name     string
		method   string
		vars     map[string]string
		body     interface{}
		fn       call
		status   int
		code     string
		contains string // something of B's that must be there (lists)
	}{
		{"list link kits", "GET", nil, nil, (*ExternalGatewayHandler).ListLinkKits, 200, "", "link-b"},
		{"roll A's kit", "POST", map[string]string{"linkID": "link-a"}, nil, (*ExternalGatewayHandler).RollLinkKit, 404, "link_not_found", ""},
		{"revoke A's kit", "DELETE", map[string]string{"linkID": "link-a"}, nil, (*ExternalGatewayHandler).RevokeLinkKit, 404, "link_not_found", ""},
		{"list routes", "GET", nil, nil, (*ExternalGatewayHandler).ListRoutes, 200, "", "b.example.com"},
		{"route on A's link", "POST", nil, map[string]interface{}{"linkId": "link-a", "domain": "new.example.com", "targetHost": "192.168.1.5"},
			(*ExternalGatewayHandler).CreateRoute, 400, "link_not_found", ""},
		{"route on A's domain", "POST", nil, map[string]interface{}{"linkId": "link-b", "domain": "a.example.com", "targetHost": "192.168.1.5"},
			(*ExternalGatewayHandler).CreateRoute, 409, "domain_taken", ""},
		{"route on A's custom domain", "POST", nil, map[string]interface{}{"linkId": "link-b", "customDomain": "a-own.net", "targetHost": "192.168.1.5"},
			(*ExternalGatewayHandler).CreateRoute, 403, "domain_unverified", ""},
		{"delete A's route", "DELETE", map[string]string{"domain": "a.example.com"}, nil, (*ExternalGatewayHandler).DeleteRoute, 404, "not_found", ""},
		{"proxy protocol on A's route", "PATCH", map[string]string{"domain": "a.example.com"}, map[string]bool{"enabled": true},
			(*ExternalGatewayHandler).SetRouteProxyProtocol, 404, "not_found", ""},
		{"voice on A's route", "PUT", map[string]string{"domain": "a.example.com"}, map[string]bool{"enabled": true},
			(*ExternalGatewayHandler).SetRouteVoice, 404, "not_found", ""},
		{"bedrock on A's route", "PUT", map[string]string{"domain": "a.example.com"}, map[string]bool{"java": true, "bedrock": true},
			(*ExternalGatewayHandler).SetRouteBedrock, 404, "not_found", ""},
		{"list custom domains", "GET", nil, nil, (*ExternalGatewayHandler).ListCustomDomains, 200, "", "b-own.net"},
		{"add A's custom domain", "POST", nil, map[string]string{"domain": "a-own.net"}, (*ExternalGatewayHandler).AddCustomDomain, 200, "", "a-own.net"},
		{"delete A's custom domain", "DELETE", map[string]string{"domain": "a-own.net"}, nil, (*ExternalGatewayHandler).DeleteCustomDomain, 404, "not_found", ""},
		{"txt token of A's domain", "POST", map[string]string{"domain": "a-own.net"}, nil, (*ExternalGatewayHandler).IssueCustomDomainTXTToken, 404, "not_found", ""},
		{"verify A's domain", "POST", map[string]string{"domain": "a-own.net"}, nil, (*ExternalGatewayHandler).VerifyCustomDomainTXT, 404, "not_found", ""},
		{"usage", "GET", nil, nil, (*ExternalGatewayHandler).GetUsage, 200, "", "1000"},
	}
	for _, admin := range []bool{false, true} {
		for _, c := range cases {
			name := c.name
			if admin {
				name += " (admin-owned key)"
			}
			t.Run(name, func(t *testing.T) {
				f := newExtFixture(t)
				rec := httptest.NewRecorder()
				c.fn(f.h, rec, extReq(c.method, "/", extTenantB, admin, c.vars, c.body))
				if rec.Code != c.status {
					t.Fatalf("status = %d, want %d: %s", rec.Code, c.status, rec.Body.String())
				}
				out := decodeExt(t, rec)
				if c.code != "" && out["code"] != c.code {
					t.Fatalf("code = %v, want %s: %s", out["code"], c.code, rec.Body.String())
				}
				body := rec.Body.String()
				for _, m := range extMarkersOfA {
					if strings.Contains(body, m) {
						t.Fatalf("answer to tenant B carries %q of tenant A: %s", m, body)
					}
				}
				if c.contains != "" && !strings.Contains(body, c.contains) {
					t.Fatalf("answer lacks B's own %q: %s", c.contains, body)
				}
				for _, dom := range append(f.gw.deleted, f.gw.proxied...) {
					if dom == "a.example.com" {
						t.Fatalf("tenant A's route was written: deleted=%v proxied=%v", f.gw.deleted, f.gw.proxied)
					}
				}
				if f.st.claim(extTenantA, "a-own.net") < 0 {
					t.Fatal("tenant A's custom domain claim was removed")
				}
				if f.st.claims[f.st.claim(extTenantA, "a-own.net")].TXTToken != extTXTA {
					t.Fatal("tenant A's TXT token was changed")
				}
			})
		}
	}
}

func TestExternalGatewayErrorsAreJSON(t *testing.T) {
	route := func(port int, host string) map[string]interface{} {
		return map[string]interface{}{"linkId": "link-b", "domain": "x.example.com", "targetHost": host, "targetPort": port}
	}
	cases := []struct {
		name   string
		body   interface{}
		mode   string
		status int
		code   string
		msg    string
	}{
		{"bad port", route(70000, "192.168.1.5"), "gateway", 400, "invalid_port", "Invalid target port"},
		{"bad target", route(25565, "bad host!"), "gateway", 400, "invalid_target", "invalid target host"},
		{"loopback v6", route(25565, "::1"), "gateway", 400, "invalid_target", "use 127.0.0.1 for this machine"},
		{"not json", "{", "gateway", 400, "invalid_request", "Invalid request body."},
		{"gateway off", route(25565, "192.168.1.5"), "ip_port", 503, "gateway_disabled", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExtFixture(t)
			f.st.settings["routing_mode"] = c.mode
			f.h.requireGateway = gate.Gates{Store: f.st}.RequireGatewayEnabled
			var r *http.Request
			if s, ok := c.body.(string); ok {
				r = extReq("POST", "/", extTenantB, false, nil, nil)
				r.Body = io.NopCloser(strings.NewReader(s))
			} else {
				r = extReq("POST", "/", extTenantB, false, nil, c.body)
			}
			rec := httptest.NewRecorder()
			f.h.CreateRoute(rec, r)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.status, rec.Body.String())
			}
			out := decodeExt(t, rec)
			keys := make([]string, 0, len(out))
			for k := range out {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, []string{"code", "error", "success"}) {
				t.Fatalf("keys = %v, want code/error/success: %s", keys, rec.Body.String())
			}
			if out["success"] != false || out["code"] != c.code {
				t.Fatalf("got %s, want code %s", rec.Body.String(), c.code)
			}
			if c.msg != "" && out["error"] != c.msg {
				t.Fatalf("error = %q, want %q", out["error"], c.msg)
			}
			if out["error"] == "" {
				t.Fatal("error message is empty")
			}
		})
	}
}

func TestExternalGatewayListShape(t *testing.T) {
	cases := []struct {
		name string
		fn   func(h *ExternalGatewayHandler, w http.ResponseWriter, r *http.Request)
	}{
		{"link kits", (*ExternalGatewayHandler).ListLinkKits},
		{"routes", (*ExternalGatewayHandler).ListRoutes},
		{"custom domains", (*ExternalGatewayHandler).ListCustomDomains},
	}
	for _, c := range cases {
		for _, owner := range []string{extTenantB, extTenantC} {
			t.Run(c.name+" "+owner, func(t *testing.T) {
				f := newExtFixture(t)
				rec := httptest.NewRecorder()
				c.fn(f.h, rec, extReq("GET", "/", owner, false, nil, nil))
				if rec.Code != 200 {
					t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
				}
				out := decodeExt(t, rec)
				items, ok := out["items"].([]interface{})
				if out["success"] != true || !ok {
					t.Fatalf("want success + items array: %s", rec.Body.String())
				}
				if want := map[string]int{extTenantB: 1, extTenantC: 0}[owner]; len(items) != want {
					t.Fatalf("items = %d, want %d: %s", len(items), want, rec.Body.String())
				}
				for _, old := range []string{"kits", "domains"} {
					if _, dup := out[old]; dup {
						t.Fatalf("list still carries %q beside items: %s", old, rec.Body.String())
					}
				}
			})
		}
	}
}

func TestExternalRegionsLists(t *testing.T) {
	f := newExtFixture(t)
	ctx := context.Background()
	for id, v := range map[string]string{
		"e1": `{"edge_id":"e1","ip":"203.0.113.9","region":"eu","wildcard":"*.eu.dylaris.com","status":"online"}`,
		"e2": `{"edge_id":"e2","ip":"203.0.113.10","region":"eu","wildcard":"*.eu.dylaris.com","status":"offline"}`,
		"e3": `{"edge_id":"e3","ip":"203.0.113.11","region":"us","wildcard":"*.us.dylaris.com","status":"offline"}`,
		"e4": `{"edge_id":"e4","ip":"203.0.113.12","status":"online"}`,
	} {
		if err := f.rdb.Set(ctx, "edge:registry:"+id, v, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	f.h.ListRegions(rec, extReq("GET", "/", extTenantB, false, nil, nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Success bool `json:"success"`
		Items   []struct {
			Region   string `json:"region"`
			Wildcard string `json:"wildcard"`
			Online   bool   `json:"online"`
		} `json:"items"`
		HosterDomains []services.HosterDomain `json:"hosterDomains"`
		CNAMETarget   string                  `json:"cnameTarget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Success || len(out.Items) != 2 {
		t.Fatalf("got %s, want two regions", rec.Body.String())
	}
	if out.Items[0].Region != "eu" || out.Items[0].Wildcard != "*.eu.dylaris.com" || !out.Items[0].Online {
		t.Fatalf("eu = %+v", out.Items[0])
	}
	if out.Items[1].Region != "us" || out.Items[1].Wildcard != "*.us.dylaris.com" || out.Items[1].Online {
		t.Fatalf("us = %+v", out.Items[1])
	}
	if len(out.HosterDomains) != 1 || out.CNAMETarget != "route" {
		t.Fatalf("route options lost: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "203.0.113.") {
		t.Fatalf("an edge address leaked: %s", rec.Body.String())
	}
}

func TestExternalUsageHasRegions(t *testing.T) {
	f := newExtFixture(t)
	f.st.regions[extTenantB] = []store.RegionUsage{
		{Region: "eu", Kind: "edge", Product: "routeonly", Bytes: 100},
		{Region: "us", Kind: "edge", Product: "routeonly", Bytes: 70},
		{Region: "eu", Kind: "edge", Product: "managed", Bytes: 50},
	}
	rec := httptest.NewRecorder()
	f.h.GetUsage(rec, extReq("GET", "/?period=2026-11", extTenantB, false, nil, nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Success bool                   `json:"success"`
		Usage   map[string]interface{} `json:"usage"`
		Regions []struct {
			Region string `json:"region"`
			Kind   string `json:"kind"`
			Bytes  int64  `json:"bytes"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Success || out.Usage["edgeBytes"] != float64(42) {
		t.Fatalf("totals lost: %s", rec.Body.String())
	}
	if len(out.Regions) != 2 || out.Regions[0].Region != "eu" || out.Regions[0].Bytes != 150 ||
		out.Regions[1].Region != "us" || out.Regions[1].Bytes != 70 {
		t.Fatalf("regions = %+v, want eu 150 then us 70", out.Regions)
	}

	t.Run("no traffic is an empty list", func(t *testing.T) {
		rec := httptest.NewRecorder()
		f.h.GetUsage(rec, extReq("GET", "/", extTenantC, false, nil, nil))
		if !strings.Contains(rec.Body.String(), `"regions":[]`) {
			t.Fatalf("want regions [], got %s", rec.Body.String())
		}
	})
	t.Run("bad period", func(t *testing.T) {
		rec := httptest.NewRecorder()
		f.h.GetUsage(rec, extReq("GET", "/?period=november", extTenantB, false, nil, nil))
		if rec.Code != 400 || decodeExt(t, rec)["code"] != "bad_request" {
			t.Fatalf("got %d %s, want 400 bad_request", rec.Code, rec.Body.String())
		}
	})
}

func (f *extStore) ListCoreLinkRoutesByOwner(ownerID string) ([]store.CoreLinkRoute, error) {
	var out []store.CoreLinkRoute
	for _, r := range f.routes {
		if r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}

// The public codes are a contract. The refusals a client acts on are driven
// through the real handlers where that is cheap, and through the mapper with
// the handlers' own strings where it is not.
func TestExternalGatewayPublicCodesAreFrozen(t *testing.T) {
	t.Run("blocked domain, real handler", func(t *testing.T) {
		f := newExtFixture(t)
		f.st.claims = append(f.st.claims, store.CustomDomainClaim{ID: 9, UserID: extTenantB, Domain: "banned.net", State: store.ClaimPermablocked, Attempts: 5})
		rec := httptest.NewRecorder()
		f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil,
			map[string]interface{}{"linkId": "link-b", "customDomain": "banned.net", "targetHost": "192.168.1.5"}))
		if rec.Code != http.StatusForbidden || decodeExt(t, rec)["code"] != "domain_blocked" {
			t.Fatalf("got %d %s, want 403 domain_blocked", rec.Code, rec.Body.String())
		}
	})
	t.Run("custom domain cap, real message", func(t *testing.T) {
		h := &GatewayHandler{d: Deps{Store: &capFakeStore{domains: map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(i64(1))}}}}
		if got := externalErrorCode(403, h.customDomainCapError("u1", 1)); got != "custom_domain_limit" {
			t.Fatalf("code = %q", got)
		}
	})
	for _, c := range []struct{ msg, code string }{
		{linkLimitMessage(5), "link_limit"},
		{ourDomainsCapMessage(3), "route_limit"},
	} {
		if got := externalErrorCode(403, c.msg); got != c.code {
			t.Errorf("%q -> %q, want %q", c.msg, got, c.code)
		}
	}
}

func TestExternalGatewayHidesInternalErrors(t *testing.T) {
	f := newExtFixture(t)
	f.gw.createRouteViaLinkErr = errors.New("dial tcp 10.9.8.7:6379: redis password wrong")
	rec := httptest.NewRecorder()
	f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil,
		map[string]interface{}{"linkId": "link-b", "domain": "new.example.com", "targetHost": "192.168.1.5"}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeExt(t, rec)
	if out["code"] != "internal" || out["error"] != "Internal error. Try again later." {
		t.Fatalf("got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "10.9.8.7") || strings.Contains(rec.Body.String(), "redis") {
		t.Fatalf("internal detail leaked: %s", rec.Body.String())
	}
}

func TestServeExternalMasksForeignServiceUnavailable(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"success":false,"error":"redis: dial tcp 10.9.8.7:6379: refused"}`))
	}
	rec := httptest.NewRecorder()
	serveExternal(rec, httptest.NewRequest("GET", "/", nil), h, nil)
	out := decodeExt(t, rec)
	if rec.Code != 503 || out["code"] != "internal" || out["error"] != "Internal error. Try again later." {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// The usage answer says whether its numbers can still grow, as of when, and
// where a managed plan stands against its one allowance.
func TestExternalUsageSaysEstimateOrFinal(t *testing.T) {
	f := newExtFixture(t)
	five := int64(5000)
	asOf := time.Date(2026, 11, 12, 10, 30, 0, 0, time.UTC)
	f.st.usage = map[string]*store.TrafficUsage{
		extTenantB: {UserID: extTenantB, EdgeBytes: 1_000_000_000_000, ManagedRelayBytes: 250_000_000_000, UpdatedAt: asOf},
	}
	f.st.limits = map[string]*models.TrafficLimit{"user:" + extTenantB: {IncludedGB: &five}}

	get := func(t *testing.T, tenant, query string) map[string]interface{} {
		t.Helper()
		rec := httptest.NewRecorder()
		f.h.GetUsage(rec, extReq("GET", "/"+query, tenant, false, nil, nil))
		if rec.Code != 200 {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return decodeExt(t, rec)
	}

	t.Run("the running month is an estimate", func(t *testing.T) {
		out := get(t, extTenantB, "")
		if out["final"] != false || out["asOf"] != "2026-11-12T10:30:00Z" {
			t.Fatalf("final %v asOf %v, want false and the last aggregation time", out["final"], out["asOf"])
		}
	})
	t.Run("a month that ended is final", func(t *testing.T) {
		if out := get(t, extTenantB, "?period=2020-01"); out["final"] != true {
			t.Fatalf("final = %v for January 2020, want true", out["final"])
		}
	})
	t.Run("a month that has not started is not final", func(t *testing.T) {
		if out := get(t, extTenantB, "?period=2999-01"); out["final"] != false {
			t.Fatalf("final = %v for a future month, want false", out["final"])
		}
	})
	t.Run("a managed plan gets its pool", func(t *testing.T) {
		pool, _ := get(t, extTenantB, "")["pool"].(map[string]interface{})
		if pool == nil || pool["includedGb"] != float64(5000) || pool["usedBytes"] != float64(1_250_000_000_000) || pool["percent"] != float64(25) {
			t.Fatalf("pool = %v, want 5000 GB included, 1.25 TB used, 25 %%", pool)
		}
		if v, ok := pool["maxPurchaseGb"]; !ok || v != nil {
			t.Fatalf("maxPurchaseGb = %v (present %v), want an explicit null: no cap", v, ok)
		}
	})
	t.Run("an earlier month has no pool: the plan may have been another", func(t *testing.T) {
		if out := get(t, extTenantB, "?period=2020-01"); out["pool"] != nil {
			t.Fatalf("pool for January 2020 = %v, want none", out["pool"])
		}
	})
	t.Run("no managed plan, no pool, nothing of another tenant", func(t *testing.T) {
		out := get(t, extTenantC, "")
		if _, ok := out["pool"]; ok {
			t.Fatalf("a tenant without the plan got a pool: %v", out["pool"])
		}
		if _, ok := out["asOf"]; ok {
			t.Fatalf("a tenant with no traffic got an asOf: %v", out["asOf"])
		}
	})
	t.Run("an allowance of none is full at the first byte", func(t *testing.T) {
		zero := int64(0)
		f.st.limits["user:"+extTenantB] = &models.TrafficLimit{IncludedGB: &zero}
		pool, _ := get(t, extTenantB, "")["pool"].(map[string]interface{})
		if pool == nil || pool["percent"] != float64(100) {
			t.Fatalf("pool = %v, want 100 %%", pool)
		}
	})
}
