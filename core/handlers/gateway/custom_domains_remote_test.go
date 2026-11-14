package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// ownDomainStore is the managed-gateway fixture (remoteReadsStore) with the
// multi-user claim table of the external fixture (extStore), so a handler that
// forgot to scope a claim by user would see the other user's.
type ownDomainStore struct {
	*remoteReadsStore
	c       *extStore
	domCaps map[string]*int64 // custom-domain limit rows by scope
	// beforeInsert runs inside RequestCustomDomainClaimCreated just before the
	// insert: a concurrent call's claim can be put in between there.
	beforeInsert func()
}

func (f *ownDomainStore) RequestCustomDomainClaimCreated(u, d string) (*store.CustomDomainClaim, bool, error) {
	if f.beforeInsert != nil {
		f.beforeInsert()
	}
	existed := f.c.claim(u, d) >= 0
	c, err := f.c.RequestCustomDomainClaim(u, d)
	return c, err == nil && !existed, err
}

func (f *ownDomainStore) GetCustomDomainClaim(u, d string) (*store.CustomDomainClaim, error) {
	return f.c.GetCustomDomainClaim(u, d)
}
func (f *ownDomainStore) RequestCustomDomainClaim(u, d string) (*store.CustomDomainClaim, error) {
	return f.c.RequestCustomDomainClaim(u, d)
}
func (f *ownDomainStore) SetCustomDomainTXTToken(id int, tok string) error {
	return f.c.SetCustomDomainTXTToken(id, tok)
}
func (f *ownDomainStore) ListCustomDomainClaimsByUser(u string) ([]store.CustomDomainClaim, error) {
	return f.c.ListCustomDomainClaimsByUser(u)
}
func (f *ownDomainStore) DeleteCustomDomainClaim(u, d string) (bool, error) {
	return f.c.DeleteCustomDomainClaim(u, d)
}
func (f *ownDomainStore) MarkCustomDomainVerified(id int) error {
	for i := range f.c.claims {
		if f.c.claims[i].ID == id {
			f.c.claims[i].State, f.c.claims[i].FailingSince = store.ClaimVerified, nil
		}
	}
	return nil
}
func (f *ownDomainStore) CountLiveCustomDomainClaims(domain, except string) (int, error) {
	n := 0
	for _, c := range f.c.claims {
		if c.Domain == domain && c.UserID != except && (c.State == store.ClaimPending || c.State == store.ClaimVerified) {
			n++
		}
	}
	return n, nil
}
func (f *ownDomainStore) ListRemoteRoutes() ([]store.RemoteRoute, error) {
	var out []store.RemoteRoute
	for _, r := range f.routes {
		out = append(out, r)
	}
	return out, nil
}
func (f *ownDomainStore) GetGatewayCustomDomainLimit(scope string) (*models.GatewayCustomDomainLimit, error) {
	if m, ok := f.domCaps[scope]; ok {
		return &models.GatewayCustomDomainLimit{Scope: scope, MaxCustomDomains: m}, nil
	}
	return nil, sql.ErrNoRows
}

// publishedTXT is DNS: the values at _dylaris-verify.<domain>.
type publishedTXT map[string][]string

func (p publishedTXT) LookupTXT(_ context.Context, host string) ([]string, error) {
	return p[strings.TrimPrefix(host, services.TXTVerifyPrefix)], nil
}
func (publishedTXT) LookupCNAME(context.Context, string) (string, error)  { return "", nil }
func (publishedTXT) LookupHost(context.Context, string) ([]string, error) { return nil, nil }

const (
	ownDomain = "play.own.net"
	userA     = "owner-1" // owns server 7
	userB     = "owner-2" // owns server 8
	srvBUUID  = "1c6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f"
	srvOpUUID = "2d6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f"
)

type ownDomainFixture struct {
	gw   *GatewayHandler
	cd   *CustomDomainHandler
	st   *ownDomainStore
	fake *remotegwtest.Fake
	dns  publishedTXT
	// base is the provider URL the RemoteGateway calls: the fake, or a stub in
	// front of it.
	base string
}

// newOwnDomainFixture: a managed-gateway platform with own domains on, user A
// on server 7, user B on server 8 and an operator server 9 without an owner.
func newOwnDomainFixture(t *testing.T) *ownDomainFixture {
	t.Helper()
	h, rs, fake := newRemoteGatewayHandler(t)
	rs.settings["gateway_custom_domains_enabled"] = "true"
	rs.servers[8] = &models.Server{ID: 8, UUID: srvBUUID, OwnerID: userB, NodeID: 3}
	rs.servers[9] = &models.Server{ID: 9, UUID: srvOpUUID, NodeID: 3}
	st := &ownDomainStore{remoteReadsStore: rs, c: &extStore{}, domCaps: map[string]*int64{}}
	h.d.Store = st
	dns := publishedTXT{}
	h.txtResolver = dns
	f := &ownDomainFixture{gw: h, st: st, fake: fake, dns: dns, base: fake.URL()}
	h.d.RemoteGateway = services.NewRemoteGateway(st, func() *remotegw.Client { return remotegw.New(f.base, fake.Key) })
	h.d.Gateway = services.NewSwitchingGateway(nil, h.d.RemoteGateway, h.d.GatewayRemote)
	f.cd = NewCustomDomainHandler(h.d, dns)
	return f
}

// dropProviderCache makes the next read of the provider's claims a real one
// (a verify drops the cache whatever it answers).
func (f *ownDomainFixture) dropProviderCache(domain string) {
	_, _ = f.gw.d.RemoteGateway.VerifyProviderClaim(context.Background(), domain)
}

// warm reads the hoster list and the claims list once, so a FailNext meets
// the call after them.
func (f *ownDomainFixture) warm(t *testing.T) {
	t.Helper()
	if _, err := f.gw.d.RemoteGateway.Regions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gw.d.RemoteGateway.ProviderCustomDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *ownDomainFixture) add(user string, admin bool, domain string) *httptest.ResponseRecorder {
	return f.call(f.cd.Add, http.MethodPost, user, admin, nil, map[string]string{"domain": domain})
}

func (f *ownDomainFixture) call(fn http.HandlerFunc, method, user string, admin bool, vars map[string]string, body interface{}) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "/", bytes.NewReader(b))
	//lint:ignore SA1029 the handlers read plain string keys.
	ctx := context.WithValue(r.Context(), "userID", user)
	//lint:ignore SA1029 same
	ctx = context.WithValue(ctx, "isAdmin", admin)
	r = r.WithContext(ctx)
	if vars != nil {
		r = mux.SetURLVars(r, vars)
	}
	rec := httptest.NewRecorder()
	fn(rec, r)
	return rec
}

func (f *ownDomainFixture) createRoute(user string, admin bool, server, domain string) *httptest.ResponseRecorder {
	return f.call(f.gw.CreateServerRoute, http.MethodPost, user, admin, map[string]string{"id": server}, map[string]string{"customDomain": domain})
}

// claim gives user a local claim on domain in state, with its own token.
func (f *ownDomainFixture) claim(user, domain, state string) string {
	tok := "dylaris-verify=local" + user + strings.ReplaceAll(domain, ".", "")
	f.st.c.claims = append(f.st.c.claims, store.CustomDomainClaim{ID: 500 + len(f.st.c.claims), UserID: user, Domain: domain, State: state, TXTToken: tok})
	return tok
}

// providerClaim makes the provider hold a claim in state and returns its TXT
// value.
func (f *ownDomainFixture) providerClaim(t *testing.T, domain, state string) string {
	t.Helper()
	d, err := f.gw.d.RemoteGateway.EnsureProviderClaim(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	if state != store.ClaimPending {
		f.fake.SetCustomDomainState(domain, state)
	}
	return d.TXTValue
}

// forgetCheck ends the local per-(user, domain) check cooldown.
func (f *ownDomainFixture) forgetCheck(user, domain string) {
	f.gw.d.Redis.Del(context.Background(), "dylaris:custom-domain:check:"+user+":"+domain)
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Code
}

// providerCalls are the fake's calls other than the cached hoster list.
func providerCalls(f *remotegwtest.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		if c != "GET /regions" {
			out = append(out, c)
		}
	}
	return out
}

func TestRemoteRouteDomainResolvesOwnDomains(t *testing.T) {
	hosters := []services.HosterDomain{{Domain: "example.com", Validation: "dns"}}
	for _, c := range []struct {
		name    string
		req     routeDomainRequest
		custom  bool
		dom     string
		isOwn   bool
		errPart string
	}{
		{"hoster subdomain", routeDomainRequest{Subdomain: "survival", HosterDomain: "example.com"}, true, "survival.example.com", false, ""},
		{"raw under hoster stays hoster", routeDomainRequest{Domain: "survival.example.com"}, true, "survival.example.com", false, ""},
		{"raw outside, own domains on", routeDomainRequest{Domain: "Play.Own.Net"}, true, "play.own.net", true, ""},
		{"raw outside, own domains off", routeDomainRequest{Domain: "play.own.net"}, false, "", false, "custom domains are not enabled"},
		{"customDomain field", routeDomainRequest{CustomDomain: "mc.own.net"}, true, "mc.own.net", true, ""},
		{"customDomain field, off", routeDomainRequest{CustomDomain: "mc.own.net"}, false, "", false, "custom domains are not enabled"},
		{"customDomain equal to a hoster domain", routeDomainRequest{CustomDomain: "example.com"}, true, "", false, "subdomain of a hoster domain"},
		{"customDomain under a hoster domain", routeDomainRequest{CustomDomain: "x.example.com"}, true, "", false, "subdomain of a hoster domain"},
		{"no dot boundary is own", routeDomainRequest{Domain: "evilexample.com"}, true, "evilexample.com", true, ""},
		{"4 labels", routeDomainRequest{CustomDomain: "a.b.own.net"}, true, "a.b.own.net", true, ""},
		{"5 labels", routeDomainRequest{CustomDomain: "a.b.c.own.net"}, true, "", false, "at most two subdomain levels"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			dom, isOwn, err := remoteRouteDomain(&req, hosters, nil, c.custom)
			if c.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), c.errPart) || isOwn {
					t.Fatalf("got %q %v %v, want error %q", dom, isOwn, err, c.errPart)
				}
				return
			}
			if err != nil || dom != c.dom || isOwn != c.isOwn {
				t.Fatalf("got %q %v %v, want %q %v", dom, isOwn, err, c.dom, c.isOwn)
			}
		})
	}
}

// A route on an own domain needs the server owner's verified local claim AND
// the provider's verified claim; each refusal says which one is missing.
func TestRemoteOwnDomainRouteNeedsBothProofs(t *testing.T) {
	f := newOwnDomainFixture(t)
	refused := func(want string) {
		t.Helper()
		rec := f.createRoute(userA, false, "7", ownDomain)
		if rec.Code != http.StatusForbidden || code(t, rec) != "domain_unverified" || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%d %s, want 403 domain_unverified naming %q", rec.Code, rec.Body, want)
		}
		if _, ok := f.st.routes[ownDomain]; ok {
			t.Fatal("a refused create left a route row")
		}
	}
	refused("Add " + ownDomain + " under Your own domains")
	if c := providerCalls(f.fake); len(c) != 0 {
		t.Fatalf("no local claim, yet the provider was asked: %v", c)
	}
	tok := f.claim(userA, ownDomain, store.ClaimPending)
	refused("This platform has not verified")
	f.dns[ownDomain] = []string{tok} // published: the gate proves it on the spot
	refused("The gateway provider has no record")
	f.providerClaim(t, ownDomain, store.ClaimPending)
	f.gw.d.RemoteGateway.ProviderCustomDomains(context.Background())
	refused("The gateway provider has not verified")
	f.fake.SetCustomDomainState(ownDomain, store.ClaimVerified)
	f.dropProviderCache(ownDomain)
	if rec := f.createRoute(userA, false, "7", ownDomain); rec.Code != http.StatusCreated {
		t.Fatalf("both proofs: %d %s", rec.Code, rec.Body)
	}
	if row := f.st.routes[ownDomain]; !row.CustomDomain || row.ServerUUID != remoteSrvUUID {
		t.Fatalf("row = %+v", row)
	}
	if c, _ := f.st.c.GetCustomDomainClaim(userA, ownDomain); c.State != store.ClaimVerified {
		t.Fatalf("the gate's live check did not record the proof: %+v", c)
	}
}

// Tenant isolation on one provider account (Review Focus 1, rewritten to the
// ruling that two users may each prove a domain): B never rides on A's proof,
// B's own proof verifies B's claim, and the route stays exclusive.
func TestRemoteOwnDomainIsolatedPerLocalUser(t *testing.T) {
	f := newOwnDomainFixture(t)
	tokA := f.claim(userA, ownDomain, store.ClaimVerified)
	f.providerClaim(t, ownDomain, store.ClaimVerified)
	before := len(f.fake.Calls())

	// B holds no claim: refused locally, the provider is not asked.
	rec := f.createRoute(userB, false, "8", ownDomain)
	if rec.Code != http.StatusForbidden || code(t, rec) != "domain_unverified" {
		t.Fatalf("B on A's proof: %d %s", rec.Code, rec.Body)
	}
	for _, c := range f.fake.Calls()[before:] {
		if c != "GET /regions" {
			t.Fatalf("B's refused create asked the provider: %v", f.fake.Calls()[before:])
		}
	}
	// An admin creating it for B's server is held to B's claim too.
	if rec := f.createRoute("admin-1", true, "8", ownDomain); rec.Code != http.StatusForbidden || code(t, rec) != "domain_unverified" {
		t.Fatalf("admin for B's server: %d %s", rec.Code, rec.Body)
	}
	if rec := f.createRoute(userA, false, "7", ownDomain); rec.Code != http.StatusCreated {
		t.Fatalf("A: %d %s", rec.Code, rec.Body)
	}

	// B adds the domain and finds only A's record published: B is not verified.
	if rec := f.call(f.cd.Add, http.MethodPost, userB, false, nil, map[string]string{"domain": ownDomain}); rec.Code != 200 {
		t.Fatalf("B add: %d %s", rec.Code, rec.Body)
	}
	f.dns[ownDomain] = []string{tokA}
	vars := map[string]string{"domain": ownDomain}
	rec = f.call(f.cd.VerifyTXT, http.MethodPost, userB, false, vars, nil)
	var v struct{ Local, Provider string }
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusConflict || v.Local != "pending" || v.Provider != "verified" {
		t.Fatalf("B verified on A's record: %d %s", rec.Code, rec.Body)
	}
	// B publishes their own value too: verified, and the route is still A's.
	claimB, _ := f.st.c.GetCustomDomainClaim(userB, ownDomain)
	f.dns[ownDomain] = []string{tokA, claimB.TXTToken}
	f.forgetCheck(userB, ownDomain)
	rec = f.call(f.cd.VerifyTXT, http.MethodPost, userB, false, vars, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 200 || v.Local != "verified" || v.Provider != "verified" {
		t.Fatalf("B's own record: %d %s", rec.Code, rec.Body)
	}
	if rec := f.createRoute(userB, false, "8", ownDomain); rec.Code != http.StatusConflict || code(t, rec) != "domain_taken" {
		t.Fatalf("B's route on A's address: %d %s", rec.Code, rec.Body)
	}
	if row := f.st.routes[ownDomain]; row.ServerUUID != remoteSrvUUID {
		t.Fatalf("the route moved to B: %+v", row)
	}
}

// An admin skips the local proof only for a server without an owner; the
// provider's proof is needed either way.
func TestRemoteOwnDomainAdminExemptOnlyForOwnerlessServers(t *testing.T) {
	f := newOwnDomainFixture(t)
	if rec := f.createRoute("admin-1", true, "9", ownDomain); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "gateway provider has no record") {
		t.Fatalf("operator server, no provider claim: %d %s", rec.Code, rec.Body)
	}
	f.providerClaim(t, ownDomain, store.ClaimVerified)
	if rec := f.createRoute("admin-1", true, "7", ownDomain); rec.Code != http.StatusForbidden || code(t, rec) != "domain_unverified" {
		t.Fatalf("admin for a tenant's server without the tenant's claim: %d %s", rec.Code, rec.Body)
	}
	if rec := f.createRoute("admin-1", true, "9", ownDomain); rec.Code != http.StatusCreated {
		t.Fatalf("operator server: %d %s", rec.Code, rec.Body)
	}
}

// The platform's own guards still hold for own domains: routes disabled, the
// per-user own-domain cap, and the hoster-domain cap does not count them.
func TestRemoteOwnDomainKeepsLocalCaps(t *testing.T) {
	zero, one := int64(0), 1
	f := newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.providerClaim(t, ownDomain, store.ClaimVerified)
	f.st.domCaps["user_default"] = &zero
	if rec := f.createRoute(userA, false, "7", ownDomain); rec.Code != http.StatusForbidden || code(t, rec) != "custom_domain_cap" {
		t.Fatalf("own-domain cap 0: %d %s", rec.Code, rec.Body)
	}
	delete(f.st.domCaps, "user_default")
	// One hoster address held at a cap of one: the own domain is not counted
	// against it, and is created.
	f.st.limits["user_default"] = &one
	f.st.routes["held.example.com"] = store.RemoteRoute{Domain: "held.example.com", ServerUUID: remoteSrvUUID, NodeID: 3}
	if rec := f.createRoute(userA, false, "7", ownDomain); rec.Code != http.StatusCreated {
		t.Fatalf("own domain at the hoster cap: %d %s", rec.Code, rec.Body)
	}
	if rec := f.call(f.gw.CreateServerRoute, http.MethodPost, userA, false, map[string]string{"id": "7"},
		map[string]string{"subdomain": "more", "hosterDomain": "example.com"}); rec.Code != http.StatusForbidden || code(t, rec) != "route_limit" {
		t.Fatalf("hoster address over the cap: %d %s", rec.Code, rec.Body)
	}
	zeroInt := 0
	f.st.limits["user_default"] = &zeroInt
	if rec := f.createRoute(userA, false, "7", ownDomain); rec.Code != http.StatusForbidden || code(t, rec) != "route_limit" {
		t.Fatalf("routes disabled: %d %s", rec.Code, rec.Body)
	}
}

// The list shows the caller's claims, each with the provider's record for that
// domain only; the provider's list is the whole platform's.
func TestRemoteClaimListShowsBothRecords(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.claim(userA, "mc.own.net", store.ClaimPending)
	f.claim(userB, "b.own.net", store.ClaimPending)
	valA := f.providerClaim(t, ownDomain, store.ClaimPending)
	valB := f.providerClaim(t, "b.own.net", store.ClaimPending)
	f.st.routes[ownDomain] = store.RemoteRoute{Domain: ownDomain, ServerUUID: remoteSrvUUID, CustomDomain: true}

	rec := f.call(f.cd.List, http.MethodGet, userA, false, nil, nil)
	var out struct {
		Success bool
		Domains []struct {
			Domain, State, TXTValue string
			Provider                *providerClaimView
		}
		Limit *struct {
			MaxCustomDomains *int64
			Used             int
		}
		ProviderError map[string]string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || len(out.Domains) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, d := range out.Domains {
		switch d.Domain {
		case ownDomain:
			if d.Provider == nil || d.Provider.State != "pending" || d.Provider.TXTValue != valA || d.Provider.TXTName != services.TXTVerifyPrefix+ownDomain || d.TXTValue == valA {
				t.Fatalf("%s: %+v provider %+v", d.Domain, d, d.Provider)
			}
		case "mc.own.net":
			if d.Provider == nil || d.Provider.State != "missing" || d.Provider.TXTValue != "" {
				t.Fatalf("no provider claim: %+v", d.Provider)
			}
		}
	}
	if strings.Contains(rec.Body.String(), valB) || strings.Contains(rec.Body.String(), "b.own.net") {
		t.Fatalf("A sees B's provider record: %s", rec.Body)
	}
	if out.Limit == nil || out.Limit.MaxCustomDomains == nil || *out.Limit.MaxCustomDomains != 0 || out.Limit.Used != 1 || out.ProviderError != nil {
		t.Fatalf("limit %+v providerError %v", out.Limit, out.ProviderError)
	}

	// The provider cannot be read: the local claims are still listed.
	f = newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimPending)
	f.fake.FailNext(2, 503, "unavailable", 0)
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	rec = f.call(f.cd.List, http.MethodGet, userA, false, nil, nil)
	log.SetOutput(prev)
	if !strings.Contains(logs.String(), "provider claims not read: provider_error") {
		t.Fatalf("log: %q", logs.String())
	}
	var raw map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	doms, _ := raw["domains"].([]interface{})
	pe, _ := raw["providerError"].(map[string]interface{})
	if rec.Code != 200 || len(doms) != 1 || pe["code"] == nil || pe["message"] == nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if p, present := doms[0].(map[string]interface{})["provider"]; !present || p != nil {
		t.Fatalf("provider = %v (present %v), want null", p, present)
	}
}

// Add makes the local claim, then the provider's; a provider refusal takes
// back a claim this call made, never one that was there before, decided by the
// insert itself.
func TestRemoteClaimAddRollsBackOnlyItsOwnClaim(t *testing.T) {
	f := newOwnDomainFixture(t)
	rec := f.add(userA, false, ownDomain)
	var out struct {
		Domain struct {
			TXTValue string
			Provider *providerClaimView
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Domain.TXTValue == "" || out.Domain.Provider == nil || out.Domain.Provider.TXTValue == "" ||
		out.Domain.Provider.TXTValue == out.Domain.TXTValue {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}

	f.warm(t)
	f.fake.FailNext(1, 409, "too_many_domains", 0)
	rec = f.add(userA, false, "new.own.net")
	if rec.Code != http.StatusConflict || code(t, rec) != "too_many_domains" || !strings.Contains(rec.Body.String(), "This platform's own-domain limit") {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.st.c.GetCustomDomainClaim(userA, "new.own.net"); err != store.ErrNoClaim {
		t.Fatal("a claim made by the refused call was kept")
	}
	if q, _ := f.st.HasOpenOutbox("new.own.net", services.OutboxOpDeleteCustomDomain); q {
		t.Fatal("a release was queued for a claim the provider refused to make")
	}
	// A claim from before the call (no provider claim yet) is kept.
	f.claim(userA, "old.own.net", store.ClaimPending)
	f.warm(t)
	f.fake.FailNext(1, 409, "too_many_domains", 0)
	if rec := f.add(userA, false, "old.own.net"); rec.Code != http.StatusConflict {
		t.Fatalf("refused again: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.st.c.GetCustomDomainClaim(userA, "old.own.net"); err != nil {
		t.Fatal("the claim from before the call was taken back")
	}
	// A concurrent Add of the same new domain inserted it between this call's
	// read and its insert: this call did not make it, so it does not take it
	// back.
	f.st.beforeInsert = func() {
		f.st.beforeInsert = nil
		f.claim(userA, "race.own.net", store.ClaimPending)
	}
	f.warm(t)
	f.fake.FailNext(1, 409, "too_many_domains", 0)
	if rec := f.add(userA, false, "race.own.net"); rec.Code != http.StatusConflict {
		t.Fatalf("race: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.st.c.GetCustomDomainClaim(userA, "race.own.net"); err != nil {
		t.Fatal("the concurrent call's claim was taken back")
	}

	// Own domains off on the provider: refused before any claim is made.
	f = newOwnDomainFixture(t)
	f.fake.SetCustomDomainsEnabled(false)
	if rec := f.add(userA, false, "off.own.net"); rec.Code != http.StatusConflict || code(t, rec) != "custom_domains_off" {
		t.Fatalf("provider off: %d %s", rec.Code, rec.Body)
	}
	if len(f.st.c.claims) != 0 || len(providerCalls(f.fake)) != 0 {
		t.Fatalf("claims %v, calls %v", f.st.c.claims, f.fake.Calls())
	}
}

// A provider claim made before a later step failed (here the answer was lost)
// is released with the local claim this call made, so it does not stay.
func TestRemoteClaimAddReleasesAnOrphanedProviderClaim(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.warm(t)
	f.fake.LoseNextAnswer()
	if rec := f.add(userA, false, ownDomain); rec.Code < 500 {
		t.Fatalf("lost answer: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.st.c.GetCustomDomainClaim(userA, ownDomain); err != store.ErrNoClaim {
		t.Fatal("local claim kept")
	}
	if q, _ := f.st.HasOpenOutbox(ownDomain, services.OutboxOpDeleteCustomDomain); !q {
		t.Fatal("the provider claim the lost call made is not released")
	}
}

// One user can hold at most three own domains the provider has not verified:
// each holds a claim in the provider's bound, which is the whole platform's.
func TestRemoteClaimAddPendingBound(t *testing.T) {
	for _, admin := range []bool{false, true} {
		f := newOwnDomainFixture(t)
		for _, d := range []string{"a.own.net", "b.own.net", "c.own.net"} {
			if rec := f.add(userA, admin, d); rec.Code != 200 {
				t.Fatalf("admin %v add %s: %d %s", admin, d, rec.Code, rec.Body)
			}
		}
		writes := countCalls(f.fake.Calls(), "POST ")
		rec := f.add(userA, admin, "d.own.net")
		if rec.Code != http.StatusConflict || code(t, rec) != "pending_domains_limit" ||
			!strings.Contains(rec.Body.String(), "Finish verifying your pending domains before adding another.") {
			t.Fatalf("admin %v fourth: %d %s", admin, rec.Code, rec.Body)
		}
		if n := countCalls(f.fake.Calls(), "POST "); n != writes {
			t.Fatalf("the refused add wrote to the provider: %v", f.fake.Calls())
		}
		if _, err := f.st.c.GetCustomDomainClaim(userA, "d.own.net"); err != store.ErrNoClaim {
			t.Fatal("the refused add left a local claim")
		}
		// Adding a domain already held is not a new pending one.
		if rec := f.add(userA, admin, "a.own.net"); rec.Code != 200 {
			t.Fatalf("admin %v re-add: %d %s", admin, rec.Code, rec.Body)
		}
		// One verified on both sides frees a slot.
		_ = f.st.MarkCustomDomainVerified(f.st.c.claims[0].ID)
		f.fake.SetCustomDomainState("a.own.net", store.ClaimVerified)
		f.dropProviderCache("a.own.net")
		if rec := f.add(userA, admin, "d.own.net"); rec.Code != 200 {
			t.Fatalf("admin %v after a verification: %d %s", admin, rec.Code, rec.Body)
		}
	}
	// The provider's list cannot be read: the bound is not skipped.
	f := newOwnDomainFixture(t)
	if _, err := f.gw.d.RemoteGateway.Regions(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.fake.FailNext(1, 503, "unavailable", 0)
	if rec := f.add(userA, false, "a.own.net"); rec.Code < 500 || len(f.st.c.claims) != 0 {
		t.Fatalf("list down: %d %s, claims %v", rec.Code, rec.Body, f.st.c.claims)
	}
}

// Adding the same domain again writes nothing to the provider, and a user
// adds at most ten a minute on the managed gateway.
func TestRemoteClaimAddIsIdempotentAndLimited(t *testing.T) {
	f := newOwnDomainFixture(t)
	for i := 0; i < remoteChecksPerMin; i++ {
		if rec := f.add(userA, false, ownDomain); rec.Code != 200 {
			t.Fatalf("add %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if n := countCalls(f.fake.Calls(), "POST /custom-domains"); n != 1 {
		t.Fatalf("provider writes = %d (%v), want 1", n, f.fake.Calls())
	}
	rec := f.add(userA, false, ownDomain)
	if rec.Code != http.StatusTooManyRequests || code(t, rec) != "rate_limited" || !strings.Contains(rec.Body.String(), "try again in a minute") {
		t.Fatalf("11th: %d %s", rec.Code, rec.Body)
	}
	if rec := f.add(userB, false, ownDomain); rec.Code != 200 {
		t.Fatalf("another user is not limited by A: %d %s", rec.Code, rec.Body)
	}
}

// With the mode unread, Add and txt-token answer 503 before doing anything.
func TestRemoteClaimHandlersRefuseWhileTheModeIsUnknown(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimPending)
	f.cd.d.GatewayRemote = func() (bool, bool) { return false, false }
	before := len(f.st.c.claims)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"add":       f.add(userA, false, "new.own.net"),
		"txt-token": f.call(f.cd.IssueTXTToken, http.MethodPost, userA, false, map[string]string{"domain": ownDomain}, nil),
	} {
		if rec.Code != http.StatusServiceUnavailable || code(t, rec) != "gateway_provider_unknown" {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(f.st.c.claims) != before || len(f.fake.Calls()) != 0 {
		t.Fatalf("claims %v, calls %v", f.st.c.claims, f.fake.Calls())
	}
}

// With the local own-domain switch off, an admin's own-domain route is
// refused on the managed gateway exactly as on the local path.
func TestOwnDomainSwitchOffRefusesAdminsInBothModes(t *testing.T) {
	local, _, _ := newProxyProtoHandler(t, false)
	body := map[string]string{"customDomain": ownDomain}
	f := newOwnDomainFixture(t)
	lrec := f.call(local.CreateServerRoute, http.MethodPost, "admin-1", true, map[string]string{"id": "7"}, body)
	f.st.settings["gateway_custom_domains_enabled"] = "false"
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.providerClaim(t, ownDomain, store.ClaimVerified)
	rrec := f.createRoute("admin-1", true, "9", ownDomain)
	const want = "custom domains are not enabled"
	if lrec.Code != http.StatusBadRequest || !strings.Contains(lrec.Body.String(), want) ||
		rrec.Code != http.StatusBadRequest || !strings.Contains(rrec.Body.String(), want) {
		t.Fatalf("local %d %s; remote %d %s", lrec.Code, lrec.Body, rrec.Code, rrec.Body)
	}
}

// Verify checks both records and says per side what is missing; the
// provider's cooldown never undoes the local verification.
func TestRemoteClaimVerifyBothSides(t *testing.T) {
	f := newOwnDomainFixture(t)
	vars := map[string]string{"domain": ownDomain}
	verify := func() (int, map[string]interface{}) {
		rec := f.call(f.cd.VerifyTXT, http.MethodPost, userA, false, vars, nil)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := verify(); code != http.StatusNotFound {
		t.Fatalf("no claim: %d", code)
	}
	tok := f.claim(userA, ownDomain, store.ClaimPending)
	f.dns[ownDomain] = []string{tok}
	// The provider holds no claim yet: verify makes one and checks it in the
	// same call (its record is not published yet); the local side passes.
	code, out := verify()
	if code != 200 || out["local"] != "verified" || out["provider"] != "pending" || !strings.Contains(out["message"].(string), "provider's TXT record was not found") {
		t.Fatalf("first: %d %v", code, out)
	}
	if _, set := out["providerCooldown"]; set {
		t.Fatalf("providerCooldown set without a provider cooldown: %v", out)
	}
	if n := countCalls(f.fake.Calls(), "POST /custom-domains/"+ownDomain+"/verify-txt"); n != 1 {
		t.Fatalf("the new claim was checked %d times, want 1", n)
	}
	pc, _ := f.gw.d.RemoteGateway.ProviderCustomDomains(context.Background())
	if pc[ownDomain].TXTValue == "" {
		t.Fatalf("no provider claim made: %v", pc)
	}
	// The local cooldown applies to the call.
	if code, out := verify(); code != http.StatusTooManyRequests || out["code"] != "verify_cooldown" {
		t.Fatalf("local cooldown: %d %v", code, out)
	}
	// Past it, the provider's own cooldown: local stays verified.
	f.forgetCheck(userA, ownDomain)
	code, out = verify()
	if code != http.StatusConflict || out["local"] != "verified" || out["provider"] != "pending" ||
		!strings.Contains(out["message"].(string), "try again in a few minutes") || out["providerCooldown"] != true {
		t.Fatalf("cooldown: %d %v", code, out)
	}
	if c, _ := f.st.c.GetCustomDomainClaim(userA, ownDomain); c.State != store.ClaimVerified {
		t.Fatalf("local claim %+v", c)
	}

	f = newOwnDomainFixture(t)
	tok = f.claim(userA, ownDomain, store.ClaimPending)
	f.providerClaim(t, ownDomain, store.ClaimPending)
	f.fake.SetTXTPublished(ownDomain, true)
	code, out = verify()
	if code != 200 || out["local"] != "pending" || out["provider"] != "verified" {
		t.Fatalf("provider only: %d %v", code, out)
	}
	f.dns[ownDomain] = []string{tok}
	f.forgetCheck(userA, ownDomain)
	code, out = verify()
	if code != 200 || out["local"] != "verified" || out["provider"] != "verified" || out["message"] != "Ownership verified. You can add routes on "+ownDomain+"." {
		t.Fatalf("both: %d %v", code, out)
	}

	// Ten checks a minute per user on the managed gateway.
	f = newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.providerClaim(t, ownDomain, store.ClaimVerified)
	for i := 0; i < remoteChecksPerMin; i++ {
		if code, out := verify(); code != 200 {
			t.Fatalf("check %d: %d %v", i, code, out)
		}
	}
	if code, out := verify(); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("over the limit: %d %v", code, out)
	}
}

// Delete is refused while a server of the caller has a route on the domain
// (the local rows, not Redis), and then releases the provider's claim only
// when no other user of the platform still holds the domain.
func TestRemoteClaimDeleteRefusedWhileRouted(t *testing.T) {
	f := newOwnDomainFixture(t)
	vars := map[string]string{"domain": ownDomain}
	del := func(user string) *httptest.ResponseRecorder {
		return f.call(f.cd.Delete, http.MethodDelete, user, false, vars, nil)
	}
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.claim(userB, ownDomain, store.ClaimVerified)
	f.st.routes[ownDomain] = store.RemoteRoute{Domain: ownDomain, ServerUUID: remoteSrvUUID, CustomDomain: true}
	if rec := del(userA); rec.Code != http.StatusConflict || code(t, rec) != "domain_in_use" {
		t.Fatalf("A while routed: %d %s", rec.Code, rec.Body)
	}
	// B's servers hold no route on it: B's claim goes; A still holds the domain.
	if rec := del(userB); rec.Code != 200 {
		t.Fatalf("B: %d %s", rec.Code, rec.Body)
	}
	if q, _ := f.st.HasOpenOutbox(ownDomain, services.OutboxOpDeleteCustomDomain); q {
		t.Fatal("released while A holds the domain")
	}
	// A's route is on its way out: A's claim goes and the release follows it.
	f.st.outbox = append(f.st.outbox, struct{ op, domain string }{services.OutboxOpDelete, ownDomain})
	if rec := del(userA); rec.Code != 200 {
		t.Fatalf("A after the route delete: %d %s", rec.Code, rec.Body)
	}
	if q, _ := f.st.HasOpenOutbox(ownDomain, services.OutboxOpDeleteCustomDomain); !q {
		t.Fatal("provider claim release not queued")
	}
	if rec := del(userA); rec.Code != http.StatusNotFound {
		t.Fatalf("again: %d", rec.Code)
	}
	if c := providerCalls(f.fake); len(c) != 0 {
		t.Fatalf("delete called the provider directly: %v", c)
	}
}

// leakValue is a provider TXT value the stub puts into every refusal it makes.
const leakValue = "dylaris-verify=11112222333344445555666677778888"

// leakyProvider sits in front of the fake and answers the next call to
// "METHOD /path" with a refusal whose message names leakValue, as the real
// provider's domain_unverified does; everything else reaches the fake.
type leakyProvider struct {
	fake *remotegwtest.Fake
	next map[string]struct {
		status int
		code   string
	}
}

func (p *leakyProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/external/gateway")
	if fl, ok := p.next[key]; ok {
		delete(p.next, key)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fl.status)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "code": fl.code,
			"error": "Add a TXT record _dylaris-verify.play.own.net with the value " + leakValue})
		return
	}
	target, _ := url.Parse(p.fake.URL())
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}

func (p *leakyProvider) refuse(key string, status int, code string) {
	p.next[key] = struct {
		status int
		code   string
	}{status, code}
}

// No TXT value of the provider (nor any local one) is in a refusal body or a
// log line, on every path that can carry a provider message: the provider's own
// route gate on a stale "verified", domain_blocked, Add's refusals, a verify
// cooldown or failure, and List's providerError.
func TestRemoteOwnDomainRefusalsCarryNoTXTValue(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	f := newOwnDomainFixture(t)
	stub := &leakyProvider{fake: f.fake, next: map[string]struct {
		status int
		code   string
	}{}}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	f.base = srv.URL

	tokA := f.claim(userA, ownDomain, store.ClaimVerified)
	val := f.providerClaim(t, ownDomain, store.ClaimVerified)
	f.gw.d.RemoteGateway.ProviderCustomDomains(context.Background()) // cached: verified
	f.fake.SetCustomDomainState(ownDomain, store.ClaimPending)       // the provider lapsed it since

	var bodies []string
	keep := func(rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		bodies = append(bodies, rec.Body.String())
		var b struct{ Code string }
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		if rec.Code != status || (code != "" && b.Code != code) {
			t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, status, code)
		}
	}
	// The provider's own gate refuses despite the cached "verified"; the row
	// the create claimed first is released again.
	keep(f.createRoute(userA, false, "7", ownDomain), http.StatusForbidden, "domain_unverified")
	if _, ok := f.st.routes[ownDomain]; ok {
		t.Fatal("the refused create left its remote_routes row")
	}
	f.fake.SetCustomDomainState(ownDomain, store.ClaimVerified)
	stub.refuse("POST /routes", 403, "domain_blocked")
	keep(f.createRoute(userA, false, "7", ownDomain), http.StatusForbidden, "domain_blocked")
	if _, ok := f.st.routes[ownDomain]; ok {
		t.Fatal("the refused create left its remote_routes row")
	}
	keep(f.createRoute(userB, false, "8", ownDomain), http.StatusForbidden, "domain_unverified")
	tokB := f.claim(userB, ownDomain, store.ClaimPending)
	keep(f.createRoute(userB, false, "8", ownDomain), http.StatusForbidden, "domain_unverified")

	// Add refusals.
	f.warm(t)
	for _, c := range []struct {
		status int
		code   string
	}{{409, "too_many_domains"}, {400, "bad_request"}, {409, "custom_domains_off"}, {409, "domain_in_use"}, {500, "internal"}} {
		stub.refuse("POST /custom-domains", c.status, c.code)
		rec := f.add(userA, false, "x"+c.code[:3]+".own.net")
		bodies = append(bodies, rec.Body.String())
		if rec.Code < 400 {
			t.Fatalf("add refused %s: %d %s", c.code, rec.Code, rec.Body)
		}
	}
	// Verify: the provider's cooldown, then a failure.
	f.dropProviderCache(ownDomain)
	f.fake.SetCustomDomainState(ownDomain, store.ClaimPending)
	vars := map[string]string{"domain": ownDomain}
	for _, c := range []struct {
		status int
		code   string
	}{{429, "verify_cooldown"}, {500, "internal"}} {
		f.forgetCheck(userA, ownDomain)
		stub.refuse("POST /custom-domains/"+ownDomain+"/verify-txt", c.status, c.code)
		keep(f.call(f.cd.VerifyTXT, http.MethodPost, userA, false, vars, nil), http.StatusConflict, "")
	}
	// List with the provider's list refused.
	f.dropProviderCache(ownDomain)
	stub.refuse("GET /custom-domains", 500, "internal")
	// The list shows A's own local record by design; only the provider's
	// message must not reach it.
	rec := f.call(f.cd.List, http.MethodGet, userA, false, nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "providerError") || strings.Contains(rec.Body.String(), leakValue) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	for _, b := range bodies {
		for _, secret := range []string{leakValue, val, tokA, tokB, "dylaris-verify="} {
			if strings.Contains(b, secret) {
				t.Fatalf("body names a TXT value %q: %s", secret, b)
			}
		}
	}
	if strings.Contains(logs.String(), "dylaris-verify=") {
		t.Fatalf("log names a TXT value: %s", logs.String())
	}
	if len(stub.next) != 0 {
		t.Fatalf("refusals never asked for: %v", stub.next)
	}
}

// The platform's own-domain count leaves out rows whose delete is queued.
func TestRemoteClaimListLimitLeavesOutQueuedDeletes(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimVerified)
	f.st.routes[ownDomain] = store.RemoteRoute{Domain: ownDomain, ServerUUID: remoteSrvUUID, CustomDomain: true}
	f.st.routes["gone.own.net"] = store.RemoteRoute{Domain: "gone.own.net", ServerUUID: srvBUUID, CustomDomain: true}
	f.st.routes["x.example.com"] = store.RemoteRoute{Domain: "x.example.com", ServerUUID: remoteSrvUUID}
	f.st.outbox = append(f.st.outbox, struct{ op, domain string }{services.OutboxOpDelete, "gone.own.net"})
	rec := f.call(f.cd.List, http.MethodGet, userA, false, nil, nil)
	var out struct{ Limit struct{ Used int } }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Limit.Used != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// provider.state is a closed set; a state the provider might add later reads
// as pending.
func TestRemoteClaimProviderStateIsAClosedSet(t *testing.T) {
	f := newOwnDomainFixture(t)
	f.claim(userA, ownDomain, store.ClaimPending)
	f.providerClaim(t, ownDomain, "suspended")
	rec := f.call(f.cd.List, http.MethodGet, userA, false, nil, nil)
	var out struct {
		Domains []struct{ Provider *providerClaimView }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Domains) != 1 || out.Domains[0].Provider.State != "pending" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
