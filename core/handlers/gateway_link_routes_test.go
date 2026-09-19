package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// linkRouteFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods CreateLinkRoute's flow touches
// (ListWarpAPIKeysByOwner, GetGatewayRouteLimit, GetSetting for
// resolveRouteDomain) are overridden. Any other call would panic - these
// tests never make one.
type linkRouteFakeStore struct {
	store.Store

	warpKeys    []store.WarpAPIKey
	warpKeysErr error

	routeLimits map[string]*models.GatewayRouteLimit

	settings map[string]string
}

func (f *linkRouteFakeStore) ListWarpAPIKeysByOwner(ownerID string) ([]store.WarpAPIKey, error) {
	return f.warpKeys, f.warpKeysErr
}

func (f *linkRouteFakeStore) GetGatewayRouteLimit(scope string) (*models.GatewayRouteLimit, error) {
	l, ok := f.routeLimits[scope]
	if !ok {
		return nil, errors.New("not found")
	}
	return l, nil
}

func (f *linkRouteFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

// linkRouteFakeGateway is a recording fake for services.GatewayProvider so
// CreateLinkRoute's success path can be asserted without a real Hub/Redis
// gateway wiring.
type linkRouteFakeGateway struct {
	createRouteViaLinkCalls []linkRouteCreateCall
	createRouteViaLinkErr   error
	createServerRouteErr    error
	createRouteServerCalls  []linkRouteCreateCall
	linkTokenFor            map[string]string
}

type linkRouteCreateCall struct {
	ownerID    string
	domain     string
	linkToken  string
	targetHost string
	targetPort int
}

func (g *linkRouteFakeGateway) CreateServerRoute(serverID uint, ownerID string, domain string, port int) error {
	g.createRouteServerCalls = append(g.createRouteServerCalls, linkRouteCreateCall{ownerID: ownerID, domain: domain, targetPort: port})
	return g.createServerRouteErr
}

func (g *linkRouteFakeGateway) CreateRouteViaLink(ownerID string, domain string, linkToken string, targetHost string, port int) error {
	g.createRouteViaLinkCalls = append(g.createRouteViaLinkCalls, linkRouteCreateCall{ownerID, domain, linkToken, targetHost, port})
	return g.createRouteViaLinkErr
}

func (g *linkRouteFakeGateway) DeleteCoreOwnedRoute(domain string) error { return nil }
func (g *linkRouteFakeGateway) DeleteRoute(domain string) error          { return nil }
func (g *linkRouteFakeGateway) DeleteServerRoutes(string) error          { return nil }
func (g *linkRouteFakeGateway) MigrateServerRoutes(serverID uint, newNodeID uint) error {
	return nil
}
func (g *linkRouteFakeGateway) LinkToken(nodeID string) string {
	if g.linkTokenFor != nil {
		if tok, ok := g.linkTokenFor[nodeID]; ok {
			return tok
		}
	}
	return "derived-token-" + nodeID
}
func (g *linkRouteFakeGateway) DiscoveryProof(nodeID string) string { return "proof-" + nodeID }

func newLinkRouteRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// routeOnlyOnFlags is a flag source with route-only switched on and nothing
// else, so the handler's own gate lets these tests reach what they test.
type routeOnlyOnFlags struct{}

func (routeOnlyOnFlags) GetSetting(key string) (string, error) {
	if key == "feature_route_only_enabled" {
		return "true", nil
	}
	return "", nil
}

// A tenant holding one route-only product: the entitlement gate passes.
func (f *linkRouteFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	one := int64(1)
	return &store.UserBilling{UserID: userID, Status: "active", MaxLinks: &one}, nil
}

func newLinkRouteHandler(fs *linkRouteFakeStore, gw *linkRouteFakeGateway, rdb *redis.Client) *GatewayHandler {
	return &GatewayHandler{state: &AppState{Store: fs, Gateway: gw, Redis: rdb, FeatureFlags: services.NewFeatureFlags(routeOnlyOnFlags{})}}
}

func linkRouteReq(userID string, body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/gateway/link-routes", bytes.NewReader(b))
	return r.WithContext(context.WithValue(r.Context(), "userID", userID))
}

// seedLinkRoute writes a route in the same format services.GetRoutesFromRedis
// reads (route:{domain} JSON + membership in sys:index:routes), so
// countOwnerRoutes sees it.
func seedLinkRoute(t *testing.T, rdb *redis.Client, domain string, route services.GatewayRoute) {
	t.Helper()
	route.Domain = domain
	data, err := json.Marshal(route)
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}
	ctx := context.Background()
	if err := rdb.Set(ctx, "route:"+domain, data, 0).Err(); err != nil {
		t.Fatalf("seed route:%s: %v", domain, err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", domain).Err(); err != nil {
		t.Fatalf("seed sys:index:routes: %v", err)
	}
}

const linkRouteUserID = "user-1"

func baseLinkRouteBody() map[string]interface{} {
	return map[string]interface{}{
		"linkId":     "link-abc",
		"domain":     "survival.example.com",
		"targetHost": "192.168.1.50",
		"targetPort": 25565,
	}
}

func baseLinkRouteStore() *linkRouteFakeStore {
	return &linkRouteFakeStore{
		warpKeys: []store.WarpAPIKey{{NodeID: "link-abc", OwnerID: linkRouteUserID}},
		// example.com is OURS in these tests, which is what makes the route cap
		// apply to the fixtures at all: the cap counts only addresses in our own
		// namespace, so a fixture on a domain we do not operate would sail past
		// every limit and prove nothing.
		settings: map[string]string{
			services.HosterDomainsSettingKey: `[{"domain":"example.com","validation":"dns"}]`,
		},
	}
}

// --- Port default/range ---

func TestCreateLinkRoute_PortDefaultAndRange(t *testing.T) {
	t.Run("targetPort omitted defaults to 25565", func(t *testing.T) {
		fs := baseLinkRouteStore()
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
		body := baseLinkRouteBody()
		delete(body, "targetPort")
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		if len(gw.createRouteViaLinkCalls) != 1 || gw.createRouteViaLinkCalls[0].targetPort != 25565 {
			t.Fatalf("calls = %+v, want targetPort=25565", gw.createRouteViaLinkCalls)
		}
	})

	t.Run("negative targetPort rejected", func(t *testing.T) {
		fs := baseLinkRouteStore()
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
		body := baseLinkRouteBody()
		body["targetPort"] = -1
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("targetPort over 65535 rejected", func(t *testing.T) {
		fs := baseLinkRouteStore()
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
		body := baseLinkRouteBody()
		body["targetPort"] = 70000
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- validateLocalTarget in the handler flow ---

func TestCreateLinkRoute_InvalidTargetHost(t *testing.T) {
	fs := baseLinkRouteStore()
	gw := &linkRouteFakeGateway{}
	h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
	body := baseLinkRouteBody()
	body["targetHost"] = ""
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(gw.createRouteViaLinkCalls) != 0 {
		t.Fatalf("expected no gateway call for an invalid target host")
	}
}

// --- Wildcard domain rejection ---

func TestCreateLinkRoute_WildcardDomainRejected(t *testing.T) {
	fs := baseLinkRouteStore()
	gw := &linkRouteFakeGateway{}
	h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
	body := baseLinkRouteBody()
	body["domain"] = "*.example.com"
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(gw.createRouteViaLinkCalls) != 0 {
		t.Fatalf("expected no gateway call for a wildcard domain")
	}
}

// --- Owner-scoped link resolve ---

func TestCreateLinkRoute_NonOwnedLinkRejected(t *testing.T) {
	fs := &linkRouteFakeStore{
		// The link kit exists but belongs to a DIFFERENT owner; resolveOwnedLinkToken
		// only matches on NodeID within the CALLER's own ListWarpAPIKeysByOwner
		// results, so a fake scoped correctly to the caller would never return
		// another owner's kit in practice. To exercise the "unknown link" branch
		// we simply return no kits at all for this caller.
		warpKeys: []store.WarpAPIKey{},
	}
	gw := &linkRouteFakeGateway{}
	h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(gw.createRouteViaLinkCalls) != 0 {
		t.Fatalf("expected no gateway call for an unknown/non-owned link")
	}
}

func TestCreateLinkRoute_EmptyLinkIDRejected(t *testing.T) {
	fs := baseLinkRouteStore()
	gw := &linkRouteFakeGateway{}
	h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
	body := baseLinkRouteBody()
	body["linkId"] = ""
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// --- Per-user route cap ---

// routeCap builds the pointer the model now carries. nil is no cap, 0 is a real
// "none": see models.GatewayRouteLimit.
func routeCap(v int) *int { return &v }

func TestCreateLinkRoute_RouteCap(t *testing.T) {
	t.Run("explicit zero override disables route creation entirely", func(t *testing.T) {
		fs := baseLinkRouteStore()
		fs.routeLimits = map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(0)}}
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("at the limit is rejected", func(t *testing.T) {
		fs := baseLinkRouteStore()
		fs.routeLimits = map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(1)}}
		rdb := newLinkRouteRedis(t)
		seedLinkRoute(t, rdb, "existing.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: linkRouteUserID})
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, rdb)
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if len(gw.createRouteViaLinkCalls) != 0 {
			t.Fatalf("expected no gateway call once the cap is reached")
		}
	})

	t.Run("under the limit proceeds", func(t *testing.T) {
		fs := baseLinkRouteStore()
		fs.routeLimits = map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(5)}}
		rdb := newLinkRouteRedis(t)
		seedLinkRoute(t, rdb, "existing.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: linkRouteUserID})
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, rdb)
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("another owner's routes do not count against this caller's cap", func(t *testing.T) {
		fs := baseLinkRouteStore()
		fs.routeLimits = map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(1)}}
		rdb := newLinkRouteRedis(t)
		seedLinkRoute(t, rdb, "someone-elses.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: "other-user"})
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, rdb)
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})

	// The cap rations OUR namespace, so a route the tenant holds on a domain they
	// own themselves must not fill it. This is the half that decides whether the
	// allowance prices what we supply or what the customer already owns.
	//
	// The mirror case - CREATING a route on a custom domain while the allowance
	// is full - is not driven here on purpose: that path runs the ownership gate,
	// which needs a claim store these fakes do not have. What it would prove
	// beyond this test is only that DomainIsOurs says no, which is tested
	// directly in services.
	t.Run("routes on their own domain do not fill the allowance", func(t *testing.T) {
		fs := baseLinkRouteStore()
		fs.routeLimits = map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(1)}}
		rdb := newLinkRouteRedis(t)
		seedLinkRoute(t, rdb, "survival.theirown.net", services.GatewayRoute{CoreOwned: true, OwnerID: linkRouteUserID})
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, rdb)
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no limit configured anywhere is unlimited", func(t *testing.T) {
		fs := baseLinkRouteStore()
		gw := &linkRouteFakeGateway{}
		h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- Happy path: created + routed via the fake Gateway ---

func TestCreateLinkRoute_Success(t *testing.T) {
	fs := baseLinkRouteStore()
	gw := &linkRouteFakeGateway{linkTokenFor: map[string]string{"link-abc": "tunnel-tok-xyz"}}
	h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if len(gw.createRouteViaLinkCalls) != 1 {
		t.Fatalf("createRouteViaLinkCalls = %+v, want exactly 1", gw.createRouteViaLinkCalls)
	}
	got := gw.createRouteViaLinkCalls[0]
	want := linkRouteCreateCall{
		ownerID: linkRouteUserID, domain: "survival.example.com", linkToken: "tunnel-tok-xyz",
		targetHost: "192.168.1.50", targetPort: 25565,
	}
	if got != want {
		t.Fatalf("CreateRouteViaLink call = %+v, want %+v", got, want)
	}

	var resp struct {
		Success bool   `json:"success"`
		Domain  string `json:"domain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || resp.Domain != "survival.example.com" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestCreateLinkRoute_GatewayFailure(t *testing.T) {
	t.Run("generic gateway error surfaces as 500", func(t *testing.T) {
		fs := baseLinkRouteStore()
		gw := &linkRouteFakeGateway{createRouteViaLinkErr: errors.New("hub unreachable")}
		h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a 'disabled' gateway error surfaces as 403", func(t *testing.T) {
		fs := baseLinkRouteStore()
		gw := &linkRouteFakeGateway{createRouteViaLinkErr: errors.New("MC port routing is disabled")}
		h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})
}

// A domain another tenant already holds is a conflict the caller can act on, not
// a server fault, and the 500 it used to land in would have read as "try again".
// Both create paths carry the same mapping; both are asserted, because they live
// in different files and the next person to touch one will not see the other.
func TestCreateRoute_DomainTakenIsAConflict(t *testing.T) {
	t.Run("route-only", func(t *testing.T) {
		gw := &linkRouteFakeGateway{createRouteViaLinkErr: services.ErrRouteDomainTaken}
		h := newLinkRouteHandler(baseLinkRouteStore(), gw, newLinkRouteRedis(t))
		rec := httptest.NewRecorder()
		h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("managed server", func(t *testing.T) {
		gw := &linkRouteFakeGateway{createServerRouteErr: services.ErrRouteDomainTaken}
		h := newLinkRouteHandler(baseLinkRouteStore(), gw, newLinkRouteRedis(t))
		body, _ := json.Marshal(map[string]interface{}{"domain": "survival.example.com", "targetPort": 25565})
		r := httptest.NewRequest("POST", "/api/servers/1/routes", bytes.NewReader(body))
		r = mux.SetURLVars(r, map[string]string{"id": "1"})
		r = r.WithContext(context.WithValue(r.Context(), "userID", linkRouteUserID))
		rec := httptest.NewRecorder()
		h.CreateServerRoute(rec, r)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestCreateLinkRoute_InvalidJSON(t *testing.T) {
	fs := baseLinkRouteStore()
	gw := &linkRouteFakeGateway{}
	h := newLinkRouteHandler(fs, gw, newLinkRouteRedis(t))
	r := httptest.NewRequest("POST", "/api/gateway/link-routes", bytes.NewReader([]byte("not json")))
	r = r.WithContext(context.WithValue(r.Context(), "userID", linkRouteUserID))
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// Route-only is a tenant path, so the reserved-name list has to apply there too.
// It resolves domains through the same helper as the server-route path; a
// divergence would let a customer claim "mc.<base>" through the door nobody
// checked.
func TestCreateLinkRoute_TenantCannotClaimAReservedLabel(t *testing.T) {
	h := newLinkRouteHandler(baseLinkRouteStore(), &linkRouteFakeGateway{}, newLinkRouteRedis(t))
	body := baseLinkRouteBody()
	body["domain"] = "mc.example.com"
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 - a tenant claimed a reserved label", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reserved") {
		t.Errorf("body = %q, want it to say the label is reserved", rec.Body.String())
	}
}

// --- Editing an existing route ---

// The allowance counts addresses HELD, and an edit is posting the same domain
// again - the one overwrite CreateRouteViaLink permits. Without separating the
// two, a tenant sitting exactly on their cap could not change the port of a
// route they own: the check refuses them over the very route it is counting,
// and tells them to buy another address to keep the same number of addresses.
func TestCreateLinkRoute_EditingAnOwnRouteDoesNotSpendTheAllowance(t *testing.T) {
	fs := baseLinkRouteStore()
	one := 1
	fs.routeLimits = map[string]*models.GatewayRouteLimit{
		"user:" + linkRouteUserID: {Scope: "user:" + linkRouteUserID, MaxRoutes: &one},
	}
	rdb := newLinkRouteRedis(t)
	seedLinkRoute(t, rdb, "survival.example.com", services.GatewayRoute{
		OwnerID: linkRouteUserID, TargetIP: "192.168.1.50", TargetPort: 25565, CoreOwned: true,
	})
	gw := &linkRouteFakeGateway{}
	h := newLinkRouteHandler(fs, gw, rdb)

	body := baseLinkRouteBody()
	body["targetPort"] = 25570
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(gw.createRouteViaLinkCalls) != 1 {
		t.Fatalf("createRouteViaLink calls = %d, want 1", len(gw.createRouteViaLinkCalls))
	}
	if got := gw.createRouteViaLinkCalls[0].targetPort; got != 25570 {
		t.Errorf("port = %d, want the edited 25570", got)
	}
}

// The cap still has to hold for a genuinely NEW address, or the exemption would
// be a way around it.
func TestCreateLinkRoute_NewDomainStillHitsTheCap(t *testing.T) {
	fs := baseLinkRouteStore()
	one := 1
	fs.routeLimits = map[string]*models.GatewayRouteLimit{
		"user:" + linkRouteUserID: {Scope: "user:" + linkRouteUserID, MaxRoutes: &one},
	}
	rdb := newLinkRouteRedis(t)
	seedLinkRoute(t, rdb, "survival.example.com", services.GatewayRoute{
		OwnerID: linkRouteUserID, TargetIP: "192.168.1.50", TargetPort: 25565, CoreOwned: true,
	})
	h := newLinkRouteHandler(fs, &linkRouteFakeGateway{}, rdb)

	body := baseLinkRouteBody()
	body["domain"] = "creative.example.com"
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, body))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a second address on a cap of one", rec.Code)
	}
}

// Someone else's domain is not an edit. The exemption keys on the OWNER, so a
// domain another tenant holds must still be refused rather than counting as
// "already mine".
func TestCreateLinkRoute_AnotherTenantsDomainIsNotAnEdit(t *testing.T) {
	fs := baseLinkRouteStore()
	one := 1
	fs.routeLimits = map[string]*models.GatewayRouteLimit{
		"user:" + linkRouteUserID: {Scope: "user:" + linkRouteUserID, MaxRoutes: &one},
	}
	rdb := newLinkRouteRedis(t)
	seedLinkRoute(t, rdb, "mine.example.com", services.GatewayRoute{
		OwnerID: linkRouteUserID, TargetIP: "192.168.1.50", TargetPort: 25565, CoreOwned: true,
	})
	seedLinkRoute(t, rdb, "survival.example.com", services.GatewayRoute{
		OwnerID: "someone-else", TargetIP: "10.0.0.9", TargetPort: 25565,
	})
	h := newLinkRouteHandler(fs, &linkRouteFakeGateway{}, rdb)

	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// A MANAGED server's route is not an edit, even on a domain the same tenant
// owns. CreateRouteViaLink overwrites only core-owned entries, so treating it
// as one would wave the allowance check through for a request the gateway then
// refuses anyway - and the refusal a tenant sees would depend on which check
// happened to fire first.
func TestCreateLinkRoute_AManagedRouteOfTheirsIsNotAnEdit(t *testing.T) {
	fs := baseLinkRouteStore()
	one := 1
	fs.routeLimits = map[string]*models.GatewayRouteLimit{
		"user:" + linkRouteUserID: {Scope: "user:" + linkRouteUserID, MaxRoutes: &one},
	}
	rdb := newLinkRouteRedis(t)
	// Same owner, same domain, but written by the hub for a managed server:
	// no core_owned flag.
	seedLinkRoute(t, rdb, "survival.example.com", services.GatewayRoute{
		OwnerID: linkRouteUserID, TargetIP: "mc_abc", TargetPort: 25565, ServerUUID: "abc",
	})
	h := newLinkRouteHandler(fs, &linkRouteFakeGateway{}, rdb)

	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, linkRouteReq(linkRouteUserID, baseLinkRouteBody()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
