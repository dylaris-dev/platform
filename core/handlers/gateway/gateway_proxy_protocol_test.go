package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// proxyProtoFakeStore backs both PATCH endpoints with a real RedisGateway on
// miniredis, so what is asserted is what the hub, the node and the edge read.
type proxyProtoFakeStore struct {
	store.Store
	settings map[string]string
	servers  map[int]*models.Server
	billing  map[string]*store.UserBilling
	routes   map[string]store.CoreLinkRoute
	setCalls int
}

func (f *proxyProtoFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *proxyProtoFakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		c := *s
		return &c, nil
	}
	return nil, errors.New("not found")
}
func (f *proxyProtoFakeStore) SetServerProxyProtocol(id int, enabled bool) error {
	f.setCalls++
	f.servers[id].ProxyProtocol = enabled
	return nil
}
func (f *proxyProtoFakeStore) GetUserBilling(id string) (*store.UserBilling, error) {
	if b, ok := f.billing[id]; ok {
		return b, nil
	}
	return nil, errors.New("no billing")
}
func (f *proxyProtoFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	out := make([]store.CoreLinkRoute, 0, len(f.routes))
	for _, r := range f.routes {
		out = append(out, r)
	}
	return out, nil
}
func (f *proxyProtoFakeStore) UpsertCoreLinkRoute(r store.CoreLinkRoute) error {
	f.routes[r.Domain] = r
	return nil
}

func newProxyProtoHandler(t *testing.T, flagOn bool) (*GatewayHandler, *proxyProtoFakeStore, *redis.Client) {
	t.Helper()
	fs := &proxyProtoFakeStore{
		settings: map[string]string{"routing_mode": "gateway"},
		servers:  map[int]*models.Server{7: {ID: 7, UUID: "srv-7", OwnerID: "owner-1"}},
		billing:  map[string]*store.UserBilling{},
		routes:   map[string]store.CoreLinkRoute{},
	}
	if flagOn {
		fs.settings[services.ProxyProtocolSettingKey] = "true"
	}
	rdb := newLinkRouteRedis(t)
	return &GatewayHandler{state: &handlers.AppState{
		Store: fs, Redis: rdb, Gateway: services.NewRedisGateway(rdb, fs, "cs"),
		FeatureFlags: services.NewFeatureFlags(fs),
	}}, fs, rdb
}

// seedRoute publishes route:<domain> for server srv-7 through tunnel tok, and
// online_link:<tok> with value presence ("" leaves the link offline).
func seedRoute(t *testing.T, rdb *redis.Client, domain, tok, presence string) {
	t.Helper()
	ctx := context.Background()
	b, _ := json.Marshal(services.GatewayRoute{TargetIP: "mc_srv-7", TargetPort: 25565, TunnelID: tok, ServerUUID: "srv-7"})
	if err := rdb.Set(ctx, "route:"+domain, b, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", domain).Err(); err != nil {
		t.Fatal(err)
	}
	if presence != "" {
		if err := rdb.Set(ctx, "online_link:"+tok, presence, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

const capsClientAddr = `{"caps":["client-addr"]}`

func patchServerProxyProtocol(h *GatewayHandler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPatch, "/api/servers/7/proxy-protocol", strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	r = r.WithContext(context.WithValue(r.Context(), "userID", "owner-1"))
	rec := httptest.NewRecorder()
	h.SetServerProxyProtocol(rec, r)
	return rec
}

func hubQueue(t *testing.T, rdb *redis.Client) []map[string]interface{} {
	t.Helper()
	raw, err := rdb.LRange(context.Background(), "dylaris:hub:queue", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, s := range raw {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestSetServerProxyProtocol_RefusedWhileTheFeatureIsOff(t *testing.T) {
	h, fs, rdb := newProxyProtoHandler(t, false)
	rec := patchServerProxyProtocol(h, `{"enabled":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "proxy_protocol_disabled") {
		t.Fatalf("status %d body %s, want 409 proxy_protocol_disabled", rec.Code, rec.Body.String())
	}
	if fs.setCalls != 0 || len(hubQueue(t, rdb)) != 0 {
		t.Fatal("a refused request changed something")
	}
}

func TestSetServerProxyProtocol_OnAndOff(t *testing.T) {
	h, fs, rdb := newProxyProtoHandler(t, true)
	seedRoute(t, rdb, "a.example.com", "tok-a", capsClientAddr)
	ctx := context.Background()
	key := "dylaris:server:srv-7:proxy_protocol"

	rec := patchServerProxyProtocol(h, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("on: status %d: %s", rec.Code, rec.Body.String())
	}
	if !fs.servers[7].ProxyProtocol {
		t.Error("column not switched on")
	}
	if v, err := rdb.Get(ctx, key).Result(); err != nil || v != "1" {
		t.Errorf("node key = %q %v, want 1", v, err)
	}
	if d, _ := rdb.TTL(ctx, key).Result(); d != -1 {
		t.Errorf("node key TTL %v, want none", d)
	}
	q := hubQueue(t, rdb)
	if len(q) != 1 || q[0]["action"] != "set_server_proxy_protocol" || q[0]["server_uuid"] != "srv-7" || q[0]["proxy_protocol"] != true {
		t.Fatalf("hub queue after on = %+v", q)
	}

	rec = patchServerProxyProtocol(h, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("off: status %d: %s", rec.Code, rec.Body.String())
	}
	if fs.servers[7].ProxyProtocol {
		t.Error("column not switched off")
	}
	if n, _ := rdb.Exists(ctx, key).Result(); n != 0 {
		t.Error("node key still present after switching off")
	}
	q = hubQueue(t, rdb)
	if len(q) != 2 || q[1]["action"] != "set_server_proxy_protocol" || q[1]["proxy_protocol"] != nil {
		t.Fatalf("hub queue after off = %+v, want a second message without proxy_protocol", q)
	}
}

func TestSetServerProxyProtocol_Refusals(t *testing.T) {
	t.Run("no enabled field", func(t *testing.T) {
		h, fs, _ := newProxyProtoHandler(t, true)
		if rec := patchServerProxyProtocol(h, `{}`); rec.Code != http.StatusBadRequest || fs.setCalls != 0 {
			t.Fatalf("status %d, setCalls %d; want 400 and nothing stored", rec.Code, fs.setCalls)
		}
	})
	t.Run("suspended owner", func(t *testing.T) {
		h, fs, rdb := newProxyProtoHandler(t, true)
		fs.billing["owner-1"] = &store.UserBilling{UserID: "owner-1", Status: "suspended"}
		if rec := patchServerProxyProtocol(h, `{"enabled":true}`); rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", rec.Code)
		}
		if fs.setCalls != 0 || len(hubQueue(t, rdb)) != 0 {
			t.Fatal("a suspended owner changed something")
		}
	})
	t.Run("unknown server", func(t *testing.T) {
		h, _, _ := newProxyProtoHandler(t, true)
		r := httptest.NewRequest(http.MethodPatch, "/api/servers/99/proxy-protocol", strings.NewReader(`{"enabled":true}`))
		r = mux.SetURLVars(r, map[string]string{"id": "99"})
		rec := httptest.NewRecorder()
		h.SetServerProxyProtocol(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
	})
}

func patchLinkRouteProxyProtocol(h *GatewayHandler, userID string, admin bool, domain, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPatch, "/api/gateway/link-routes/"+domain+"/proxy-protocol", strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"domain": domain})
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", admin)
	rec := httptest.NewRecorder()
	h.SetLinkRouteProxyProtocol(rec, r.WithContext(ctx))
	return rec
}

func TestSetLinkRouteProxyProtocol(t *testing.T) {
	h, fs, rdb := newProxyProtoHandler(t, true)
	if err := h.state.Gateway.CreateRouteViaLink("owner-1", "play.example.com", "tunnel-1", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), "online_link:tunnel-1", capsClientAddr, 0).Err(); err != nil {
		t.Fatal(err)
	}
	live := func() services.GatewayRoute {
		t.Helper()
		v, err := rdb.Get(context.Background(), "route:play.example.com").Result()
		if err != nil {
			t.Fatal(err)
		}
		var r services.GatewayRoute
		if err := json.Unmarshal([]byte(v), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	if rec := patchLinkRouteProxyProtocol(h, "stranger", false, "play.example.com", `{"enabled":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger: status %d, want 404", rec.Code)
	}
	if live().ProxyProtocol || fs.routes["play.example.com"].ProxyProtocol {
		t.Fatal("a stranger switched the route")
	}

	if rec := patchLinkRouteProxyProtocol(h, "owner-1", false, "play.example.com", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("owner: status %d: %s", rec.Code, rec.Body.String())
	}
	if !live().ProxyProtocol || !fs.routes["play.example.com"].ProxyProtocol {
		t.Fatal("owner's switch did not reach the row and the live route")
	}
	// The repair loop must not flip it back.
	services.NewRouteRepublisher(fs, rdb).RunOnce(context.Background())
	if !live().ProxyProtocol {
		t.Fatal("republisher flipped the switch back")
	}

	if rec := patchLinkRouteProxyProtocol(h, "owner-1", false, "missing.example.com", `{"enabled":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing route: status %d, want 404", rec.Code)
	}
}

// The platform setting gates switching forwarding ON only. With it off, an
// owner can still switch their own forwarding off, and everything it wrote is
// cleared; switching on stays refused and changes nothing.
func TestSetServerProxyProtocol_OffAlwaysAllowed(t *testing.T) {
	h, fs, rdb := newProxyProtoHandler(t, false)
	ctx := context.Background()
	key := "dylaris:server:srv-7:proxy_protocol"
	fs.servers[7].ProxyProtocol = true
	if err := rdb.Set(ctx, key, "1", 0).Err(); err != nil {
		t.Fatal(err)
	}

	if rec := patchServerProxyProtocol(h, `{"enabled":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("on while the setting is off: status %d, want 409", rec.Code)
	}
	if fs.setCalls != 0 || len(hubQueue(t, rdb)) != 0 {
		t.Fatal("a refused enable changed something")
	}

	rec := patchServerProxyProtocol(h, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("off while the setting is off: status %d: %s", rec.Code, rec.Body.String())
	}
	if fs.servers[7].ProxyProtocol {
		t.Error("column not cleared")
	}
	if n, _ := rdb.Exists(ctx, key).Result(); n != 0 {
		t.Error("node key not cleared")
	}
	q := hubQueue(t, rdb)
	if len(q) != 1 || q[0]["action"] != "set_server_proxy_protocol" || q[0]["server_uuid"] != "srv-7" || q[0]["proxy_protocol"] != nil {
		t.Fatalf("hub queue = %+v, want one switch-off message", q)
	}

	// Same authorization as before: a cut-off owner is still refused.
	fs.billing["owner-1"] = &store.UserBilling{UserID: "owner-1", Status: "suspended"}
	if rec := patchServerProxyProtocol(h, `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("suspended owner switching off: status %d, want 403", rec.Code)
	}
}

func TestSetLinkRouteProxyProtocol_OffAlwaysAllowed(t *testing.T) {
	h, fs, rdb := newProxyProtoHandler(t, false)
	gw := h.state.Gateway
	if err := gw.CreateRouteViaLink("owner-1", "play.example.com", "tunnel-1", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	// Switched on while the setting was still on.
	if err := gw.SetCoreOwnedRouteProxyProtocol("play.example.com", true); err != nil {
		t.Fatal(err)
	}
	liveOn := func() bool {
		t.Helper()
		v, err := rdb.Get(context.Background(), "route:play.example.com").Result()
		if err != nil {
			t.Fatal(err)
		}
		var r services.GatewayRoute
		if err := json.Unmarshal([]byte(v), &r); err != nil {
			t.Fatal(err)
		}
		return r.ProxyProtocol
	}

	if rec := patchLinkRouteProxyProtocol(h, "owner-1", false, "play.example.com", `{"enabled":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("on while the setting is off: status %d, want 409", rec.Code)
	}
	if rec := patchLinkRouteProxyProtocol(h, "stranger", false, "play.example.com", `{"enabled":false}`); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger switching off: status %d, want 404", rec.Code)
	}
	if !liveOn() || !fs.routes["play.example.com"].ProxyProtocol {
		t.Fatal("a refused request changed the route")
	}

	if rec := patchLinkRouteProxyProtocol(h, "owner-1", false, "play.example.com", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("owner switching off: status %d: %s", rec.Code, rec.Body.String())
	}
	if liveOn() || fs.routes["play.example.com"].ProxyProtocol {
		t.Fatal("switch-off did not clear the row and the live route")
	}
}

// Enabling needs every route's link to have announced client-addr: the edge
// sends the header-less stream through any other link, and a server expecting
// PROXY cannot be joined that way. Every refusal changes nothing.
func TestSetServerProxyProtocol_RefusedWithoutLinkCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *redis.Client)
		code string
	}{
		{"no routes", func(*testing.T, *redis.Client) {}, "no_routes"},
		{"one route without the cap", func(t *testing.T, rdb *redis.Client) {
			seedRoute(t, rdb, "a.example.com", "tok-a", capsClientAddr)
			seedRoute(t, rdb, "b.example.com", "tok-b", `{"caps":["other"]}`)
		}, "link_without_client_addr"},
		{"old link value 1", func(t *testing.T, rdb *redis.Client) {
			seedRoute(t, rdb, "a.example.com", "tok-a", "1")
		}, "link_without_client_addr"},
		{"link offline", func(t *testing.T, rdb *redis.Client) {
			seedRoute(t, rdb, "a.example.com", "tok-a", "")
		}, "link_without_client_addr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs, rdb := newProxyProtoHandler(t, true)
			tc.seed(t, rdb)
			rec := patchServerProxyProtocol(h, `{"enabled":true}`)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status %d body %s, want 409 %s", rec.Code, rec.Body.String(), tc.code)
			}
			if fs.setCalls != 0 || len(hubQueue(t, rdb)) != 0 {
				t.Fatal("a refused enable changed something")
			}
		})
	}
	t.Run("message names the route", func(t *testing.T) {
		h, _, rdb := newProxyProtoHandler(t, true)
		seedRoute(t, rdb, "b.example.com", "tok-b", "1")
		rec := patchServerProxyProtocol(h, `{"enabled":true}`)
		if !strings.Contains(rec.Body.String(), "The link of route b.example.com has not reported that it can pass the player IP. Update the link, then try again.") {
			t.Fatalf("body %s", rec.Body.String())
		}
	})
	t.Run("all routes capable", func(t *testing.T) {
		h, fs, rdb := newProxyProtoHandler(t, true)
		seedRoute(t, rdb, "a.example.com", "tok-a", capsClientAddr)
		seedRoute(t, rdb, "b.example.com", "tok-b", `{"caps":["x","client-addr"]}`)
		if rec := patchServerProxyProtocol(h, `{"enabled":true}`); rec.Code != http.StatusOK || !fs.servers[7].ProxyProtocol {
			t.Fatalf("status %d body %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("redis error fails closed", func(t *testing.T) {
		h, fs, _ := newProxyProtoHandler(t, true)
		h.state.Redis = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
		rec := patchServerProxyProtocol(h, `{"enabled":true}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "link_check_failed") {
			t.Fatalf("status %d body %s, want 409 link_check_failed", rec.Code, rec.Body.String())
		}
		if fs.setCalls != 0 {
			t.Fatal("a refused enable stored something")
		}
	})
}

func TestSetLinkRouteProxyProtocol_RefusedWithoutLinkCap(t *testing.T) {
	h, fs, rdb := newProxyProtoHandler(t, true)
	if err := h.state.Gateway.CreateRouteViaLink("owner-1", "play.example.com", "tunnel-1", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), "online_link:tunnel-1", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	rec := patchLinkRouteProxyProtocol(h, "owner-1", false, "play.example.com", `{"enabled":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "link_without_client_addr") {
		t.Fatalf("status %d body %s, want 409 link_without_client_addr", rec.Code, rec.Body.String())
	}
	if fs.routes["play.example.com"].ProxyProtocol {
		t.Fatal("a refused enable switched the route")
	}
	if err := rdb.Set(context.Background(), "online_link:tunnel-1", capsClientAddr, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if rec := patchLinkRouteProxyProtocol(h, "owner-1", false, "play.example.com", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("with the cap: status %d body %s", rec.Code, rec.Body.String())
	}
}
