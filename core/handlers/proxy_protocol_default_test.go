package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
)

// proxyDefaultStore adds what the real-IP default reads and writes to the
// install fixture.
type proxyDefaultStore struct {
	*installFakeStore
	installs  []models.SubServerInstall
	ppWrites  []bool
	ppErr     error
	activeErr error
}

func (f *proxyDefaultStore) ListSubServerInstalls(int) ([]models.SubServerInstall, error) {
	return f.installs, nil
}
func (f *proxyDefaultStore) SetServerProxyProtocol(_ int, enabled bool) error {
	if f.ppErr != nil {
		return f.ppErr
	}
	f.ppWrites = append(f.ppWrites, enabled)
	f.server.ProxyProtocol = enabled
	return nil
}
func (f *proxyDefaultStore) UpdateServerActiveSubServer(int, string) error {
	return f.activeErr
}

// newProxyDefaultTest is a NEW Paper server (never set up) on a node that
// writes the config, behind a link that passes the player address, with the
// platform setting on and gateway routing: the one shape that defaults to on.
func newProxyDefaultTest(t *testing.T) (*ServerHandler, *proxyDefaultStore, *redis.Client) {
	t.Helper()
	h, fs, rdb := newInstallTest(t)
	fs.server.InstallerType = ""
	fs.server.MinecraftVersion = ""
	fs.server.BuildNumber = ""
	fs.server.ActiveSubServer = ""
	ps := &proxyDefaultStore{installFakeStore: fs, activeErr: errors.New("stop here")}
	fs.settings["routing_mode"] = "gateway"
	fs.settings[services.ProxyProtocolSettingKey] = "true"
	h.state.Store = ps
	h.state.FeatureFlags = services.NewFeatureFlags(ps)
	h.state.Gateway = services.NewRedisGateway(rdb, ps, "test-cluster-secret")
	ctx := context.Background()
	rdb.Set(ctx, "dylaris:discovery:node-lib", `{"releaseVersion":"`+proxyProtocolDefaultSince+`"}`, 0)
	rdb.Set(ctx, "online_link:"+services.DeriveLinkToken("node-lib", "test-cluster-secret"), `{"caps":["client-addr"]}`, 0)
	return h, ps, rdb
}

func setupWith(h *ServerHandler, installer map[string]string, createsRoute bool) *httptest.ResponseRecorder {
	body := setupBody(installer)
	if createsRoute {
		body["createsRoute"] = true
	}
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", body, false))
	return rec
}

func setupPaper(h *ServerHandler) *httptest.ResponseRecorder {
	return setupWith(h, map[string]string{"type": "paper", "version": "1.21.4"}, true)
}

func hubProxyActions(t *testing.T, rdb *redis.Client) []map[string]any {
	t.Helper()
	raw, err := rdb.LRange(context.Background(), "dylaris:hub:queue", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, s := range raw {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil && m["action"] == "set_server_proxy_protocol" {
			out = append(out, m)
		}
	}
	return out
}

func pendingProxy(t *testing.T, rdb *redis.Client) bool {
	t.Helper()
	ok, err := rdb.HExists(context.Background(), "proxy_protocol_default:pending", "srv-uuid").Result()
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// pendingIsDefault reports whether the recorded request came from the
// new-server default - the only kind whose "no" switches the server off.
func pendingIsDefault(t *testing.T, rdb *redis.Client) bool {
	t.Helper()
	raw, err := rdb.HGet(context.Background(), "proxy_protocol_default:pending", "srv-uuid").Result()
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Default bool `json:"default"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p.Default
}

func TestSetupDefaultsANewServerToProxyProtocol(t *testing.T) {
	h, ps, rdb := newProxyDefaultTest(t)
	if rec := setupPaper(h); rec.Code != http.StatusOK {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	inst := queuedInstaller(t, rdb)
	if len(inst) != 1 || inst[0]["proxyProtocol"] != true {
		t.Fatalf("queued %v, want proxyProtocol true", inst)
	}
	if len(ps.ppWrites) != 1 || !ps.ppWrites[0] {
		t.Fatalf("column writes %v, want [true]", ps.ppWrites)
	}
	if n, _ := rdb.Exists(context.Background(), services.ServerProxyProtocolKey("srv-uuid")).Result(); n != 1 {
		t.Fatal("node key not set")
	}
	if acts := hubProxyActions(t, rdb); len(acts) != 1 || acts[0]["proxy_protocol"] != true {
		t.Fatalf("hub actions %v", acts)
	}
	if !pendingProxy(t, rdb) || !pendingIsDefault(t, rdb) {
		t.Fatal("the request was not recorded for the sweep as the default")
	}
}

// Each case breaks one thing the handler reads; none may switch anything.
func TestSetupLeavesProxyProtocolOptIn(t *testing.T) {
	cases := map[string]func(*ServerHandler, *proxyDefaultStore, *redis.Client){
		"existing server back in pending_setup": func(_ *ServerHandler, ps *proxyDefaultStore, _ *redis.Client) {
			ps.installs = []models.SubServerInstall{{SubServerName: "lobby"}}
		},
		"installer type already recorded": func(_ *ServerHandler, ps *proxyDefaultStore, _ *redis.Client) {
			ps.server.InstallerType = "paper"
		},
		"server already has a route": func(_ *ServerHandler, _ *proxyDefaultStore, rdb *redis.Client) {
			b, _ := json.Marshal(services.GatewayRoute{TargetIP: "mc_srv-uuid", ServerUUID: "srv-uuid"})
			rdb.Set(context.Background(), "route:play.example.com", b, 0)
			rdb.SAdd(context.Background(), "sys:index:routes", "play.example.com")
		},
		"linked to a platform proxy": func(_ *ServerHandler, ps *proxyDefaultStore, _ *redis.Client) {
			id := 2
			ps.server.ProxyID = &id
		},
		"node one release behind": func(_ *ServerHandler, _ *proxyDefaultStore, rdb *redis.Client) {
			rdb.Set(context.Background(), "dylaris:discovery:node-lib", `{"releaseVersion":"2026.10.21"}`, 0)
		},
		"node release unknown": func(_ *ServerHandler, _ *proxyDefaultStore, rdb *redis.Client) {
			rdb.Del(context.Background(), "dylaris:discovery:node-lib")
		},
		"link without client-addr": func(_ *ServerHandler, _ *proxyDefaultStore, rdb *redis.Client) {
			rdb.Set(context.Background(), "online_link:"+services.DeriveLinkToken("node-lib", "test-cluster-secret"), "1", 0)
		},
		"link learned by the hub lacks client-addr": func(_ *ServerHandler, ps *proxyDefaultStore, _ *redis.Client) {
			ps.node.LinkToken = "enrolled-link"
		},
		"platform setting off": func(h *ServerHandler, ps *proxyDefaultStore, _ *redis.Client) {
			ps.settings[services.ProxyProtocolSettingKey] = "false"
			h.state.FeatureFlags = services.NewFeatureFlags(ps)
		},
		"routing both": func(_ *ServerHandler, ps *proxyDefaultStore, _ *redis.Client) {
			ps.settings["routing_mode"] = "both"
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h, ps, rdb := newProxyDefaultTest(t)
			breakIt(h, ps, rdb)
			assertSetupOptIn(t, h, ps, rdb, setupPaper(h))
		})
	}
	t.Run("no route requested (older panel, or no domain)", func(t *testing.T) {
		h, ps, rdb := newProxyDefaultTest(t)
		assertSetupOptIn(t, h, ps, rdb, setupWith(h, map[string]string{"type": "paper", "version": "1.21.4"}, false))
	})
	t.Run("vanilla", func(t *testing.T) {
		h, ps, rdb := newProxyDefaultTest(t)
		assertSetupOptIn(t, h, ps, rdb, setupWith(h, map[string]string{"type": "vanilla", "version": "1.21.4"}, true))
	})
}

func assertSetupOptIn(t *testing.T, _ *ServerHandler, ps *proxyDefaultStore, rdb *redis.Client, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	inst := queuedInstaller(t, rdb)
	if len(inst) != 1 {
		t.Fatalf("queued %v", inst)
	}
	if _, ok := inst[0]["proxyProtocol"]; ok || len(ps.ppWrites) != 0 || len(hubProxyActions(t, rdb)) != 0 || pendingProxy(t, rdb) {
		t.Fatalf("switched: installer %v, column %v", inst[0], ps.ppWrites)
	}
}

// A server that is already set up never gets the default, whatever it
// installs next (asked directly: SetupServer also checks the sub-server cap
// with the node for such a server, which this fixture has no node for).
func TestProxyDefaultOnlyForAFirstSetup(t *testing.T) {
	h, ps, _ := newProxyDefaultTest(t)
	srv := *ps.server
	if !h.proxyProtocolDefault(context.Background(), &srv, ps.node, "paper", "1.21.4", true) {
		t.Fatal("the fixture must be eligible")
	}
	srv.Status = "stopped"
	if h.proxyProtocolDefault(context.Background(), &srv, ps.node, "paper", "1.21.4", true) {
		t.Fatal("an installed server defaulted to on")
	}
}

// A switch that could not be written is not half on, and the node is not
// asked to configure a server Core never switched.
func TestSetupWithoutTheSwitchSendsNoProxyRequest(t *testing.T) {
	h, ps, rdb := newProxyDefaultTest(t)
	ps.ppErr = errors.New("db down")
	if rec := setupPaper(h); rec.Code != http.StatusOK {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	if inst := queuedInstaller(t, rdb); len(inst) != 1 || inst[0]["proxyProtocol"] != nil {
		t.Fatalf("queued %v", inst)
	}
	if n, _ := rdb.Exists(context.Background(), services.ServerProxyProtocolKey("srv-uuid")).Result(); n != 0 {
		t.Fatal("node key set without the column")
	}
	if pendingProxy(t, rdb) {
		t.Fatal("a request nobody sent stays pending")
	}
}

// When the command cannot be queued, the default is undone - even when the
// client is already gone.
func TestSetupQueueFailureSwitchesBackOff(t *testing.T) {
	h, ps, rdb := newProxyDefaultTest(t)
	// The node's command stream is a plain string, so XADD fails.
	rdb.Set(context.Background(), "dylaris:node:node-lib:cmds", "not-a-stream", 0)
	if rec := setupPaper(h); rec.Code != http.StatusInternalServerError {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	if len(ps.ppWrites) != 2 || !ps.ppWrites[0] || ps.ppWrites[1] {
		t.Fatalf("column writes %v, want [true false]", ps.ppWrites)
	}
	if n, _ := rdb.Exists(context.Background(), services.ServerProxyProtocolKey("srv-uuid")).Result(); n != 0 {
		t.Fatal("node key left on")
	}
	if pendingProxy(t, rdb) {
		t.Fatal("request left pending")
	}
}

func TestSwitchProxyProtocolOffSurvivesACancelledRequest(t *testing.T) {
	h, ps, rdb := newProxyDefaultTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rdb.Set(context.Background(), services.ServerProxyProtocolKey("srv-uuid"), "1", 0)
	h.switchProxyProtocolOff(ctx, ps.server)
	if len(ps.ppWrites) != 1 || ps.ppWrites[0] {
		t.Fatalf("column writes %v", ps.ppWrites)
	}
	if n, _ := rdb.Exists(context.Background(), services.ServerProxyProtocolKey("srv-uuid")).Result(); n != 0 {
		t.Fatal("node key left on")
	}
}

// --- a server whose switch is ON ---

func newProxyOnTest(t *testing.T) (*ServerHandler, *proxyDefaultStore, *redis.Client) {
	h, ps, rdb := newProxyDefaultTest(t)
	ps.server.ProxyProtocol = true
	ps.server.InstallerType = "paper"
	ps.server.ActiveSubServer = "main"
	return h, ps, rdb
}

func TestInstallWhileOnRefusesSoftwareThatCannotExpectIt(t *testing.T) {
	for _, inst := range []map[string]string{
		{"type": "vanilla", "version": "1.21.4"},
		{"type": "fabric", "version": "1.21.4"},
		{"type": "paper", "version": "1.18.2"},
		{"type": "upload"},
	} {
		h, ps, rdb := newProxyOnTest(t)
		rec := setupWith(h, inst, true)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "proxy_protocol_on") {
			t.Fatalf("%v: status %d %s, want 409 proxy_protocol_on", inst, rec.Code, rec.Body.String())
		}
		if len(queuedInstaller(t, rdb)) != 0 || ps.setupWrites != 0 || len(ps.ppWrites) != 0 {
			t.Fatalf("%v: something was written after the refusal", inst)
		}
	}
}

// Supported software on a server that is on carries the request, so the node
// writes (or confirms) the setting - including after a config wipe.
func TestInstallWhileOnCarriesTheRequest(t *testing.T) {
	h, ps, rdb := newProxyOnTest(t)
	rec := setupWith(h, map[string]string{"type": "paper", "version": "1.21.4"}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if inst := queuedInstaller(t, rdb); len(inst) != 1 || inst[0]["proxyProtocol"] != true {
		t.Fatalf("queued %v", inst)
	}
	if !pendingProxy(t, rdb) || len(ps.ppWrites) != 0 {
		t.Fatalf("pending %v, column writes %v", pendingProxy(t, rdb), ps.ppWrites)
	}
	if pendingIsDefault(t, rdb) {
		t.Fatal("a server already on was recorded as the default; a failed install would switch it off")
	}
}

func reinstall(h *ServerHandler, installer map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ReinstallServer(rec, installRequest("reinstall", map[string]any{"installer": installer, "javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, false))
	return rec
}

func TestReinstallWhileOn(t *testing.T) {
	h, ps, rdb := newProxyOnTest(t)
	ps.server.Status = "stopped"
	rec := reinstall(h, map[string]string{"type": "fabric", "version": "1.21.4"})
	if rec.Code != http.StatusConflict || len(queuedInstaller(t, rdb)) != 0 || ps.setupWrites != 0 {
		t.Fatalf("fabric: status %d %s, queued %d", rec.Code, rec.Body.String(), len(queuedInstaller(t, rdb)))
	}
	rec = reinstall(h, map[string]string{"type": "paper", "version": "1.21.4"})
	if rec.Code != http.StatusOK {
		t.Fatalf("paper: status %d %s", rec.Code, rec.Body.String())
	}
	if inst := queuedInstaller(t, rdb); len(inst) != 1 || inst[0]["proxyProtocol"] != true || !pendingProxy(t, rdb) {
		t.Fatalf("paper: queued %v", inst)
	}
	if pendingIsDefault(t, rdb) {
		t.Fatal("paper: a reinstall was recorded as the default; a failed download would switch the server off")
	}

	// Off, nothing changes for a reinstall.
	h2, ps2, rdb2 := newProxyOnTest(t)
	ps2.server.Status = "stopped"
	ps2.server.ProxyProtocol = false
	if rec := reinstall(h2, map[string]string{"type": "fabric", "version": "1.21.4"}); rec.Code != http.StatusOK {
		t.Fatalf("off: status %d %s", rec.Code, rec.Body.String())
	}
	if inst := queuedInstaller(t, rdb2); len(inst) != 1 || inst[0]["proxyProtocol"] != nil || pendingProxy(t, rdb2) {
		t.Fatalf("off: queued %v", inst)
	}
}

func switchTo(h *ServerHandler, sub string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.SwitchSubServer(rec, installRequest("switch", map[string]any{"subServerName": sub}, false))
	return rec
}

func TestSwitchWhileOnNeedsAConfiguredSubServer(t *testing.T) {
	h, ps, rdb := newProxyOnTest(t)
	ps.server.Status = "stopped"
	ps.installs = []models.SubServerInstall{{SubServerName: "main"}, {SubServerName: "lobby"}}
	if rec := switchTo(h, "lobby"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "proxy_protocol_on") {
		t.Fatalf("unknown sub-server: status %d %s", rec.Code, rec.Body.String())
	}
	if err := services.MarkProxyProtocolSubConfigured(context.Background(), rdb, "srv-uuid", "lobby"); err != nil {
		t.Fatal(err)
	}
	// Past the guard: the fixture's store stops the switch right after it.
	if rec := switchTo(h, "lobby"); rec.Code == http.StatusConflict {
		t.Fatalf("configured sub-server refused: %s", rec.Body.String())
	}
	ps.server.ProxyProtocol = false
	if rec := switchTo(h, "other"); rec.Code == http.StatusConflict {
		t.Fatalf("switch off, still refused: %s", rec.Body.String())
	}
}

func TestLinkToProxyRefusedWhileOn(t *testing.T) {
	h, ps, _ := newProxyOnTest(t)
	link := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.LinkServerToProxy(rec, installRequest("proxy", map[string]any{"proxyId": 2}, false))
		return rec
	}
	if rec := link(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "proxy_protocol_on") {
		t.Fatalf("on: status %d %s", rec.Code, rec.Body.String())
	}
	ps.server.ProxyProtocol = false
	if rec := link(); rec.Code == http.StatusConflict {
		t.Fatalf("off: refused: %s", rec.Body.String())
	}
}
