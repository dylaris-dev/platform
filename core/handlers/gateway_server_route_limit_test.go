package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// The per-user gateway route cap is one allowance shared by both doors:
// countOwnerRoutes counts EVERY route whose owner is this user, whichever
// endpoint created it. Only /api/gateway/link-routes ever consulted it.
//
// So a tenant capped at N could create unlimited routes through
// POST /api/servers/{id}/routes, and - the sharper half - the explicit
// "disabled" mode an operator sets via PUT /api/users/{id}/route-limit to stop
// an abusive tenant was enforced on one endpoint and ignored on the other.
//
// RedisGateway.CreateServerRoute's "limit counts skipped - Hub enforces
// uniqueness in its DB" is true and about a different thing: uniqueness stops
// two routes sharing a domain, it does not bound how many one account holds.

func serverRouteReq(userID string, isAdmin bool, body map[string]interface{}) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/servers/1/routes", bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	return r.WithContext(ctx)
}

func TestCreateServerRouteHonorsTheRouteLimit(t *testing.T) {
	tests := []struct {
		name     string
		limits   map[string]*models.GatewayRouteLimit
		existing int
		isAdmin  bool
		// domain defaults to one of OURS. Set it to a domain we do not operate to
		// exercise the exemption.
		domain     string
		wantStatus int
		wantCreate bool
	}{
		{
			name:       "no limit configured at all",
			existing:   5,
			wantStatus: http.StatusCreated,
			wantCreate: true,
		},
		{
			name:       "under the user override",
			limits:     map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(3)}},
			existing:   2,
			wantStatus: http.StatusCreated,
			wantCreate: true,
		},
		{
			name:       "at the user override",
			limits:     map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(3)}},
			existing:   3,
			wantStatus: http.StatusForbidden,
		},
		{
			// The mode an operator picks to stop an abusive tenant outright.
			name:       "route creation explicitly disabled for this user",
			limits:     map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(0)}},
			existing:   0,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "the platform-wide user_default applies too",
			limits:     map[string]*models.GatewayRouteLimit{"user_default": {MaxRoutes: routeCap(1)}},
			existing:   1,
			wantStatus: http.StatusForbidden,
		},
		{
			// The cap is a tenant allowance; an admin registering the platform's
			// own routes must not be blocked by user_default. Same asymmetry
			// resolveRouteDomain already applies to the reserved-name list.
			name:       "an admin is not capped",
			limits:     map[string]*models.GatewayRouteLimit{"user_default": {MaxRoutes: routeCap(1)}},
			existing:   9,
			isAdmin:    true,
			wantStatus: http.StatusCreated,
			wantCreate: true,
		},
		{
			// The allowance rations OUR namespace. Nine held addresses against a
			// cap of one is as over as it gets, and it still must not stop a
			// domain the customer owns - that one costs the allowance nothing.
			name:       "a full allowance does not block the customer's own domain",
			limits:     map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(1)}},
			existing:   9,
			domain:     "survival.theirown.net",
			wantStatus: http.StatusCreated,
			wantCreate: true,
		},
		{
			// "Disabled" is an operator stopping this tenant, not a full
			// allowance, so it holds on every domain including their own.
			name:       "a disabled tenant is stopped on their own domain too",
			limits:     map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(0)}},
			domain:     "survival.theirown.net",
			wantStatus: http.StatusForbidden,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newLinkRouteRedis(t)
			// Held as SERVER routes, in the shape the hub publishes them: no
			// owner_id, only the server they serve. Seeding them with an
			// owner_id - which the hub never writes - is what let managed
			// routes go uncounted while this test stayed green.
			for i := 0; i < tc.existing; i++ {
				seedLinkRoute(t, rdb, "held"+string(rune('a'+i))+".example.com",
					services.GatewayRoute{TunnelID: "tok", TargetIP: "mc_srv-1", ServerUUID: "srv-1"})
			}
			// example.com is ours here: the cap only counts addresses in our own
			// namespace, so without this the fixtures would sail past every limit.
			// Custom domains ON, because two cases below route a domain the
			// customer owns. They used to reach the create through the raw `domain`
			// field, which took any FQDN from anyone - so the fixture never had to
			// say whether the feature was even enabled. It does now: a tenant's raw
			// domain is the custom-domain path, switch and ownership proof included.
			fs := &linkRouteFakeStore{routeLimits: tc.limits, settings: map[string]string{
				services.HosterDomainsSettingKey: `[{"domain":"example.com","validation":"dns"}]`,
				"gateway_custom_domains_enabled": "true",
			}, servers: map[string][]models.Server{linkRouteUserID: {{ID: 1, UUID: "srv-1"}}}}
			gw := &linkRouteFakeGateway{}
			h := newLinkRouteHandler(fs, gw, rdb)

			domain := tc.domain
			if domain == "" {
				domain = "new.example.com"
			}
			rec := httptest.NewRecorder()
			h.CreateServerRoute(rec, serverRouteReq(linkRouteUserID, tc.isAdmin, map[string]interface{}{
				"domain": domain, "targetPort": 25565,
			}))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// A refusal must land BEFORE the route is queued: a 403 that still
			// created the route would be worse than no check at all.
			if got := len(gw.createRouteServerCalls); (got > 0) != tc.wantCreate {
				t.Fatalf("gateway create calls = %d, wantCreate = %v", got, tc.wantCreate)
			}
		})
	}
}

// A server route belongs to the server's OWNER, whoever with network.write on
// the server creates it: the owner's allowance pays for it, and the owner's
// claim proves its domain. The hub publishes managed routes with no owner, so
// the server is the only thing that ties one back to an account - a claim
// armed for a member instead could never find the route to remove it.
func TestAServerRouteIsTheServerOwners(t *testing.T) {
	const member = "member-1"
	rdb := newLinkRouteRedis(t)
	seedLinkRoute(t, rdb, "helda.example.com",
		services.GatewayRoute{TunnelID: "tok", TargetIP: "mc_srv-1", ServerUUID: "srv-1"})
	fs := &linkRouteFakeStore{
		routeLimits: map[string]*models.GatewayRouteLimit{"user:" + linkRouteUserID: {MaxRoutes: routeCap(1)}},
		settings: map[string]string{
			services.HosterDomainsSettingKey: `[{"domain":"example.com","validation":"dns"}]`,
			"gateway_custom_domains_enabled": "true",
		},
		servers: map[string][]models.Server{linkRouteUserID: {{ID: 1, UUID: "srv-1"}}},
	}

	t.Run("the owner's allowance applies to a member", func(t *testing.T) {
		gw := &linkRouteFakeGateway{}
		rec := httptest.NewRecorder()
		newLinkRouteHandler(fs, gw, rdb).CreateServerRoute(rec, serverRouteReq(member, false,
			map[string]interface{}{"domain": "new.example.com", "targetPort": 25565}))
		if rec.Code != http.StatusForbidden || len(gw.createRouteServerCalls) != 0 {
			t.Fatalf("status %d, creates %d: a member spent around the owner's full allowance", rec.Code, len(gw.createRouteServerCalls))
		}
	})

	t.Run("the route is filed under the owner", func(t *testing.T) {
		gw := &linkRouteFakeGateway{}
		rec := httptest.NewRecorder()
		newLinkRouteHandler(fs, gw, rdb).CreateServerRoute(rec, serverRouteReq(member, false,
			map[string]interface{}{"domain": "survival.theirown.net", "targetPort": 25565}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if got := gw.createRouteServerCalls[0].ownerID; got != linkRouteUserID {
			t.Fatalf("route filed under %q, want the server owner %q", got, linkRouteUserID)
		}
	})
}

// Routes on a tenant's own domains cost us no address, and that is why they
// had no bound at all: each one is a claim the verifier queries a
// tenant-chosen nameserver for, and a key every edge loads.
func TestRoutesOnOwnDomainsAreCapped(t *testing.T) {
	cases := []struct {
		name    string
		setting string
		held    int
		want    int
	}{
		{"the default cap", "", 50, http.StatusForbidden},
		{"under the default cap", "", 49, http.StatusCreated},
		{"an operator cap", "2", 2, http.StatusForbidden},
		{"zero means none", "0", 0, http.StatusForbidden},
		{"no cap", "unlimited", 80, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newLinkRouteRedis(t)
			for i := 0; i < tc.held; i++ {
				seedLinkRoute(t, rdb, fmt.Sprintf("s%d.theirown.net", i),
					services.GatewayRoute{TunnelID: "tok", TargetIP: "mc_srv-1", ServerUUID: "srv-1"})
			}
			fs := &linkRouteFakeStore{settings: map[string]string{
				services.HosterDomainsSettingKey: `[{"domain":"example.com","validation":"dns"}]`,
				"gateway_custom_domains_enabled": "true",
				SettingMaxCustomDomains:          tc.setting,
			}, servers: map[string][]models.Server{linkRouteUserID: {{ID: 1, UUID: "srv-1"}}}}
			gw := &linkRouteFakeGateway{}
			rec := httptest.NewRecorder()
			newLinkRouteHandler(fs, gw, rdb).CreateServerRoute(rec, serverRouteReq(linkRouteUserID, false,
				map[string]interface{}{"domain": "new.theirown.net", "targetPort": 25565}))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if (len(gw.createRouteServerCalls) > 0) != (tc.want == http.StatusCreated) {
				t.Fatalf("creates = %d for status %d", len(gw.createRouteServerCalls), rec.Code)
			}
		})
	}
}
