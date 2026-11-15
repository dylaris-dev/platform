package gateway

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services/voicecfg"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
)

const voiceSvcPath = "config/voicechat/voicechat-server.properties"

type voiceSwitchStore struct {
	store.Store
	server   models.Server
	row      *store.VoicePort
	poolFull bool
	perms    map[string]bool
	global   string
	routing  string // routing_mode; "" reads as ip_port, the gateway off
	billing  *store.UserBilling
}

func (f *voiceSwitchStore) GetServerByID(id int) (*models.Server, error) {
	if id != f.server.ID {
		return nil, errors.New("not found")
	}
	s := f.server
	return &s, nil
}
func (f *voiceSwitchStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "tok"}, nil
}
func (f *voiceSwitchStore) GetVoicePort(int) (*store.VoicePort, error) {
	if f.row == nil {
		return nil, nil
	}
	r := *f.row
	return &r, nil
}
func (f *voiceSwitchStore) AllocateVoicePort(p store.VoicePort, min, _ int) (store.VoicePort, error) {
	if f.poolFull {
		return store.VoicePort{}, store.ErrVoicePoolFull
	}
	p.ServerUUID, p.Port = f.server.UUID, min
	f.row = &p
	return p, nil
}
func (f *voiceSwitchStore) DeleteVoicePort(int) error { f.row = nil; return nil }
func (f *voiceSwitchStore) ListVoicePorts() ([]store.VoicePort, error) {
	if f.row == nil {
		return nil, nil
	}
	return []store.VoicePort{*f.row}, nil
}
func (f *voiceSwitchStore) ListLinkRouteVoice() ([]store.LinkRouteVoice, error) { return nil, nil }
func (f *voiceSwitchStore) GetVoicePermission(scope string) (*bool, error) {
	if v, ok := f.perms[scope]; ok {
		return &v, nil
	}
	return nil, nil
}
func (f *voiceSwitchStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *voiceSwitchStore) GetSetting(k string) (string, error) {
	if k == services.VoiceChatSettingKey && f.global != "" {
		return f.global, nil
	}
	if k == "routing_mode" && f.routing != "" {
		return f.routing, nil
	}
	return "", sql.ErrNoRows
}
func (f *voiceSwitchStore) GetUserBilling(string) (*store.UserBilling, error) {
	if f.billing == nil {
		return nil, errors.New("no billing")
	}
	return f.billing, nil
}

type voiceSwitchEnv struct {
	h    *GatewayHandler
	fs   *voiceSwitchStore
	mr   *miniredis.Miniredis
	node *handlertest.VoiceNode
}

// newVoiceSwitch is a server that meets every enable precondition: allowed
// globally, a node on voiceNodeSince, a route under an online edge's wildcard,
// a live relay in that region, an active sub-server, and the svc config.
func newVoiceSwitch(t *testing.T) *voiceSwitchEnv {
	t.Helper()
	fs := &voiceSwitchStore{
		server:  models.Server{ID: 7, UUID: "srv-7", NodeID: 7, OwnerID: "owner-1", ActiveSubServer: "survival"},
		global:  "true",
		routing: "gateway",
	}
	st, mr, node := voiceNodeState(t, fs, voiceSvcPath, "svc_before.properties")
	for k, v := range map[string]string{
		"dylaris:discovery:tok":  `{"id":"tok","releaseVersion":"` + voiceNodeSince + `"}`,
		"route:a.eu.dylaris.com": `{"target_ip":"mc_srv-7","target_port":25565,"server_uuid":"srv-7"}`,
		"edge:registry:e1":       `{"edge_id":"e1","status":"online","region":"eu","wildcard":"*.eu.dylaris.com"}`,
		"voice:registry:r1":      `{"voice_id":"r1","region":"eu","ip":"203.0.113.9"}`,
	} {
		mr.Set(k, v)
	}
	mr.SAdd("sys:index:routes", "a.eu.dylaris.com")
	mr.SAdd("sys:voices", "r1")
	return &voiceSwitchEnv{h: &GatewayHandler{d: st}, fs: fs, mr: mr, node: node}
}

func (e *voiceSwitchEnv) put(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/servers/7/voice/switch", strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	rec := httptest.NewRecorder()
	e.h.SetServerVoice(rec, r)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (e *voiceSwitchEnv) voiceKeys() []string {
	var out []string
	for _, k := range e.mr.Keys() {
		if strings.HasPrefix(k, "voice:port:") || strings.HasPrefix(k, "sys:index:voice:") || strings.HasSuffix(k, ":voice") {
			out = append(out, k)
		}
	}
	return out
}

func (e *voiceSwitchEnv) written() string {
	return e.node.WrittenString()
}

// The enable preconditions, in the order they are checked. Case i breaks
// precondition i AND every later one, so the code answered also proves that
// nothing after it was asked first.
var voiceRefusals = []struct {
	code   string
	break_ func(e *voiceSwitchEnv)
}{
	{"voice_not_allowed", func(e *voiceSwitchEnv) { e.fs.global = "" }},
	{"node_too_old", func(e *voiceSwitchEnv) {
		e.mr.Set("dylaris:discovery:tok", `{"id":"tok","releaseVersion":"2026.10.21.9"}`)
	}},
	{"node_too_old", func(e *voiceSwitchEnv) { // voicewire v1: cannot name our servers
		e.mr.Set("dylaris:discovery:tok", `{"id":"tok","releaseVersion":"2026.10.22"}`)
	}},
	{"no_routes", func(e *voiceSwitchEnv) { e.mr.Del("route:a.eu.dylaris.com") }},
	{"no_relay", func(e *voiceSwitchEnv) {
		e.mr.Set("voice:registry:r1", `{"voice_id":"r1","region":"us","ip":"203.0.113.9"}`)
	}},
	{"no_sub_server", func(e *voiceSwitchEnv) { e.fs.server.ActiveSubServer = "" }},
	{"voice_mod_missing", func(e *voiceSwitchEnv) { e.node.Path = "nothing/here" }},
	{"voice_mod_ambiguous", func(e *voiceSwitchEnv) { e.node.Also = "config/plasmovoice/server/config.toml" }},
	{"voice_pool_full", func(e *voiceSwitchEnv) { e.fs.poolFull = true }},
}

func TestVoiceEnableRefusals(t *testing.T) {
	for i, c := range voiceRefusals {
		t.Run(c.code, func(t *testing.T) {
			e := newVoiceSwitch(t)
			// voice_mod_missing must not be undone by the ambiguous breaker,
			// which only adds a second path to a node that serves the first.
			for j := len(voiceRefusals) - 1; j >= i; j-- {
				if c.code == "voice_mod_missing" && voiceRefusals[j].code == "voice_mod_ambiguous" {
					continue
				}
				voiceRefusals[j].break_(e)
			}
			rec, out := e.put(t, `{"enabled":true}`)
			if rec.Code != http.StatusConflict || out["code"] != c.code {
				t.Fatalf("status %d body %s, want 409 %s", rec.Code, rec.Body.String(), c.code)
			}
			if e.fs.row != nil || len(e.voiceKeys()) != 0 || e.written() != "" {
				t.Fatalf("a refused enable wrote something: row %+v keys %v file %q", e.fs.row, e.voiceKeys(), e.written())
			}
		})
	}
}

func TestVoiceModMissingMessage(t *testing.T) {
	e := newVoiceSwitch(t)
	e.node.Path = "nothing/here"
	_, out := e.put(t, `{"enabled":true}`)
	if out["message"] != "Install Simple Voice Chat or Plasmo Voice and start the server once." {
		t.Fatalf("message %q", out["message"])
	}
}

func TestVoiceEnableHappyPath(t *testing.T) {
	e := newVoiceSwitch(t)
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	v, _ := out["voice"].(map[string]interface{})
	if v["enabled"] != true || v["port"] != float64(26000) || v["host"] != "a.eu.dylaris.com" || v["mod"] != "svc" || v["allowed"] != true {
		t.Fatalf("voice %v", v)
	}
	if e.fs.row == nil || e.fs.row.Port != 26000 || e.fs.row.Region != "eu" || len(e.fs.row.UplinkKey) != 32 {
		t.Fatalf("row %+v", e.fs.row)
	}
	for _, k := range []string{"voice:port:eu:26000", "dylaris:server:srv-7:voice"} {
		if !e.mr.Exists(k) {
			t.Fatalf("%s not written; keys %v", k, e.mr.Keys())
		}
	}
	if want := voicecfg.SvcEnable(handlertest.VoiceFixture(t, "svc_before.properties"), "a.eu.dylaris.com", 26000); e.written() != want {
		t.Fatalf("config written:\n%s\nwant:\n%s", e.written(), want)
	}
}

func TestVoiceNodeWriteFailureRollsBack(t *testing.T) {
	e := newVoiceSwitch(t)
	e.node.FailWrite = true
	// A voice_host the owner set by hand, which a write-back would blank.
	e.node.File = strings.Replace(handlertest.VoiceFixture(t, "svc_after.properties"), "voice_host=a.eu.dylaris.com:26000", "voice_host=mine.example.org:4000", 1)
	rec, _ := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d body %s, want 502", rec.Code, rec.Body.String())
	}
	if e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("not rolled back: row %+v keys %v", e.fs.row, e.voiceKeys())
	}
	// The failed write changed nothing on the node, so the rollback must not
	// write the disabled config back: that would strip hand-set settings.
	writes := e.node.WriteCount()
	if writes != 1 {
		t.Fatalf("node saw %d write requests, want 1 (the enable's own)", writes)
	}
}

func TestVoiceDisableAllowedWhileFlagOff(t *testing.T) {
	e := newVoiceSwitch(t)
	e.fs.global = ""
	e.fs.perms = map[string]bool{"server:7": false}
	e.node.File = handlertest.VoiceFixture(t, "svc_after.properties")
	row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Mod: "svc", Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
	e.fs.row = &row
	if err := services.PublishVoice(context.Background(), e.h.d.Redis, row); err != nil {
		t.Fatal(err)
	}
	rec, out := e.put(t, `{"enabled":false}`)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("not disabled: row %+v keys %v", e.fs.row, e.voiceKeys())
	}
	if want := voicecfg.SvcDisable(e.node.File); e.written() != want {
		t.Fatalf("config written back:\n%s\nwant:\n%s", e.written(), want)
	}
}

func TestVoiceDisableWithoutRowTouchesNothing(t *testing.T) {
	e := newVoiceSwitch(t)
	rec, out := e.put(t, `{"enabled":false}`)
	if rec.Code != http.StatusOK || out["restartRequired"] != false || e.written() != "" {
		t.Fatalf("status %d body %s written %q", rec.Code, rec.Body.String(), e.written())
	}
}

func TestVoiceSwitchRefusedWhileSuspended(t *testing.T) {
	e := newVoiceSwitch(t)
	e.fs.billing = &store.UserBilling{Status: "suspended"}
	rec, _ := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusForbidden || e.fs.row != nil || len(e.voiceKeys()) != 0 || e.written() != "" {
		t.Fatalf("enable: status %d, row %+v keys %v; want 403 and nothing written", rec.Code, e.fs.row, e.voiceKeys())
	}

	// Off is never refused (owner decision 2026-10-22): the row and keys go.
	e = newVoiceSwitch(t)
	e.fs.billing = &store.UserBilling{Status: "suspended"}
	e.disableExisting(t, "suspended")
}

// The switch is registered without RequireGatewayEnabled, so the handler is
// the gate: on is refused with the gateway off, off still works.
func TestVoiceSwitchGatewayOff(t *testing.T) {
	e := newVoiceSwitch(t)
	e.fs.routing = ""
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusServiceUnavailable || out["error"] != "gateway_disabled" || e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("enable: status %d body %s; want 503 gateway_disabled and nothing written", rec.Code, rec.Body.String())
	}
	e = newVoiceSwitch(t)
	e.fs.routing = ""
	e.disableExisting(t, "gateway off")
}

// disableExisting gives the server voice, turns it off and wants it gone.
func (e *voiceSwitchEnv) disableExisting(t *testing.T, why string) {
	t.Helper()
	row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Mod: "svc", Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
	e.fs.row = &row
	if err := services.PublishVoice(context.Background(), e.h.d.Redis, row); err != nil {
		t.Fatal(err)
	}
	rec, out := e.put(t, `{"enabled":false}`)
	if rec.Code != http.StatusOK || out["restartRequired"] != true || e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("disable (%s): status %d body %s, row %+v keys %v; want 200 and the voice gone", why, rec.Code, rec.Body.String(), e.fs.row, e.voiceKeys())
	}
}

func TestVoiceSwitchNeedsTheField(t *testing.T) {
	e := newVoiceSwitch(t)
	if rec, _ := e.put(t, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: a body without enabled must not read as off", rec.Code)
	}
}

func TestGetServerVoiceReportsAllowed(t *testing.T) {
	cases := []struct {
		name        string
		global      string
		perms       map[string]bool
		row         bool
		wantAllowed bool
	}{
		{"allowed globally", "true", nil, false, true},
		{"denied for the server beats global", "true", map[string]bool{"server:7": false}, true, false},
		{"allowed for the owner beats global off", "", map[string]bool{"user:owner-1": true}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newVoiceSwitch(t)
			e.fs.global, e.fs.perms = c.global, c.perms
			if c.row {
				e.fs.row = &store.VoicePort{ServerID: 7, Region: "eu", Port: 26003, Mod: "svc", Host: "a.eu.dylaris.com"}
			}
			r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/servers/7/voice", nil), map[string]string{"id": "7"})
			rec := httptest.NewRecorder()
			e.h.GetServerVoice(rec, r)
			var out struct {
				Voice voiceView `json:"voice"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Voice.Allowed != c.wantAllowed || out.Voice.Enabled != c.row || (c.row && out.Voice.Port != 26003) {
				t.Fatalf("voice %+v", out.Voice)
			}
		})
	}
}

type voicePermAdminStore struct {
	store.Store
	rows map[string]bool
}

func (f *voicePermAdminStore) SetVoicePermission(scope string, allowed bool) error {
	f.rows[scope] = allowed
	return nil
}
func (f *voicePermAdminStore) DeleteVoicePermission(scope string) error {
	delete(f.rows, scope)
	return nil
}

func TestVoicePermissionAdminAPI(t *testing.T) {
	fs := &voicePermAdminStore{rows: map[string]bool{}}
	h := &GatewayHandler{d: Deps{Store: fs}}
	put := func(body string) int {
		rec := httptest.NewRecorder()
		h.SetVoicePermission(rec, httptest.NewRequest(http.MethodPut, "/api/admin/voice-permissions", strings.NewReader(body)))
		return rec.Code
	}
	for body, want := range map[string]int{
		`{"scope":"role:3","allowed":true}`:   200,
		`{"scope":"user:u1","allowed":false}`: 200,
		`{"scope":"global","allowed":true}`:   400,
		`{"scope":"server:x","allowed":true}`: 400,
		`{"scope":"server:7"}`:                400,
	} {
		if got := put(body); got != want {
			t.Errorf("PUT %s = %d, want %d", body, got, want)
		}
	}
	if len(fs.rows) != 2 || fs.rows["role:3"] != true || fs.rows["user:u1"] != false {
		t.Fatalf("rows %v", fs.rows)
	}
	rec := httptest.NewRecorder()
	h.DeleteVoicePermission(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/voice-permissions?scope=role:3", nil))
	if _, ok := fs.rows["role:3"]; rec.Code != 200 || ok {
		t.Fatalf("delete: status %d rows %v", rec.Code, fs.rows)
	}
	rec = httptest.NewRecorder()
	h.DeleteVoicePermission(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/voice-permissions?scope=global", nil))
	if rec.Code != 400 {
		t.Fatalf("delete global: status %d, want 400", rec.Code)
	}
}

// On the managed gateway without its client, or with the mode unread, voice
// cannot be turned on. Turning it off still works on the former: the row and
// the node config go, this Redis is not written. With the mode unread off is
// 503 too: it cannot tell which side holds the voice (review M-8).
func TestVoiceSwitchRemoteAndUnknownMode(t *testing.T) {
	for _, tc := range []struct {
		name         string
		remote, know bool
		status       int
		code         string
	}{
		{"remote", true, true, http.StatusConflict, "gateway_remote"},
		{"unknown", false, false, http.StatusServiceUnavailable, "gateway_provider_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVoiceSwitch(t)
			e.h.d.GatewayRemote = func() (bool, bool) { return tc.remote, tc.know }
			rec, out := e.put(t, `{"enabled":true}`)
			if rec.Code != tc.status || out["code"] != tc.code || e.fs.row != nil || len(e.voiceKeys()) != 0 {
				t.Fatalf("enable: %d %s; want %d %s and nothing written", rec.Code, rec.Body.String(), tc.status, tc.code)
			}

			e = newVoiceSwitch(t)
			e.h.d.GatewayRemote = func() (bool, bool) { return tc.remote, tc.know }
			row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Mod: "svc", Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
			e.fs.row = &row
			// A leftover key from local days stays: the remote mode never writes here.
			if err := services.PublishVoice(context.Background(), e.h.d.Redis, row); err != nil {
				t.Fatal(err)
			}
			keys := len(e.voiceKeys())
			rec, out = e.put(t, `{"enabled":false}`)
			if !tc.remote {
				if rec.Code != http.StatusServiceUnavailable || out["code"] != "gateway_provider_unknown" || e.fs.row == nil || len(e.voiceKeys()) != keys {
					t.Fatalf("disable with the mode unread: %d %s, row %+v", rec.Code, rec.Body.String(), e.fs.row)
				}
				return
			}
			if rec.Code != http.StatusOK || out["restartRequired"] != true || e.fs.row != nil {
				t.Fatalf("disable: %d %s, row %+v", rec.Code, rec.Body.String(), e.fs.row)
			}
			if len(e.voiceKeys()) != keys {
				t.Fatalf("voice keys changed in %s mode: %v", tc.name, e.voiceKeys())
			}
		})
	}
}

// --- A hosted server whose only address is its own domain (local mode). ---

const plasmoPath = "config/plasmovoice/server/config.toml"

// newHostedOwnDomain is newVoiceSwitch for a server whose only address is
// odDomain, the eu edge at edgeAddr, and the shared checker over a fake
// resolver.
func newHostedOwnDomain(t *testing.T) (*voiceSwitchEnv, *odResolver) {
	t.Helper()
	e := newVoiceSwitch(t)
	e.mr.Del("route:a.eu.dylaris.com")
	e.mr.SRem("sys:index:routes", "a.eu.dylaris.com")
	e.addHostedRoute(odDomain)
	e.mr.Set("edge:registry:e1", `{"edge_id":"e1","status":"online","region":"eu","wildcard":"*.eu.dylaris.com","ip":"`+edgeAddr+`"}`)
	res := &odResolver{cname: map[string]string{}, a: map[string][]netip.Addr{}, srv: map[string][]*net.SRV{}}
	e.h.d.Pointing = odChecker(e.h.d.Redis, res)
	return e, res
}

func (e *voiceSwitchEnv) addHostedRoute(domain string) {
	e.mr.Set("route:"+domain, `{"target_ip":"mc_srv-7","target_port":25565,"server_uuid":"srv-7"}`)
	e.mr.SAdd("sys:index:routes", domain)
}

func TestHostedOwnDomainVoiceWritesOurName(t *testing.T) {
	for _, c := range []struct {
		mod, path, fixture string
		enable             func(content, host string, port int) string
		line               string
	}{
		{"svc", voiceSvcPath, "svc_before.properties", voicecfg.SvcEnable, "voice_host=" + ourVoice + ":26000"},
		{"plasmo", plasmoPath, "plasmo_default.toml", voicecfg.PlasmoEnable, `ip = "` + ourVoice + `"`},
	} {
		t.Run(c.mod, func(t *testing.T) {
			e, res := newHostedOwnDomain(t)
			e.node.Path, e.node.File = c.path, handlertest.VoiceFixture(t, c.fixture)
			res.cname[odDomain] = "route.eu.dylaris.com"
			res.a["route.eu.dylaris.com"] = addrs(edgeAddr)
			rec, out := e.put(t, `{"enabled":true}`)
			if rec.Code != http.StatusOK || out["restartRequired"] != true {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			if v, _ := out["voice"].(map[string]interface{}); v["host"] != ourVoice || v["mod"] != c.mod {
				t.Fatalf("voice %v", v)
			}
			if e.fs.row == nil || e.fs.row.Host != ourVoice || e.fs.row.Region != "eu" {
				t.Fatalf("row %+v", e.fs.row)
			}
			if key, _ := e.mr.Get("dylaris:server:srv-7:voice"); !strings.Contains(key, `"region":"eu"`) || !strings.Contains(key, `"host":"`+ourVoice+`"`) {
				t.Fatalf("server key %s", key)
			}
			want := c.enable(handlertest.VoiceFixture(t, c.fixture), ourVoice, 26000)
			if e.written() != want || !strings.Contains(want, c.line) {
				t.Fatalf("config written:\n%s\nwant (with %q):\n%s", e.written(), c.line, want)
			}
		})
	}
}

func TestHostedOwnDomainVoiceRefusalsWriteNothing(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(e *voiceSwitchEnv, res *odResolver)
		status  int
		code    string
		message string
	}{
		{"points elsewhere", func(_ *voiceSwitchEnv, r *odResolver) { r.a[odDomain] = addrs(foreignAddr) },
			409, "no_routes", notPointingText(odDomain)},
		{"no record", func(*voiceSwitchEnv, *odResolver) {}, 409, "no_routes", notPointingText(odDomain)},
		{"lookup failed", func(_ *voiceSwitchEnv, r *odResolver) {
			r.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		},
			503, "dns_lookup_failed", "The lookup of " + odDomain + " failed right now. Nothing was changed. Try again in a moment."},
		{"rate limited", func(e *voiceSwitchEnv, r *odResolver) {
			r.a[odDomain] = addrs(edgeAddr)
			e.h.d.PointingLimiter = httpx.NewIPRateLimiter()
			for i := 0; i < pointingChecksPerMin; i++ {
				e.h.d.PointingLimiter.Allow("user:owner-1", pointingChecksPerMin)
			}
		}, 429, "rate_limited", "Too many DNS checks. Wait a minute."},
		{"no checker", func(e *voiceSwitchEnv, _ *odResolver) { e.h.d.Pointing = nil },
			503, "dns_lookup_failed", "The lookup of " + odDomain + " failed right now. Nothing was changed. Try again in a moment."},
		{"no routes at all", func(e *voiceSwitchEnv, _ *odResolver) { e.mr.Del("route:" + odDomain) },
			409, "no_routes", "Voice chat needs an address on one of the platform's domains. Add a route first."},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, res := newHostedOwnDomain(t)
			c.setup(e, res)
			r := httptest.NewRequest(http.MethodPut, "/api/servers/7/voice/switch", strings.NewReader(`{"enabled":true}`))
			r = mux.SetURLVars(r, map[string]string{"id": "7"})
			r = r.WithContext(context.WithValue(r.Context(), "userID", "owner-1"))
			rec := httptest.NewRecorder()
			e.h.SetServerVoice(rec, r)
			var out map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if rec.Code != c.status || out["code"] != c.code || out["message"] != c.message {
				t.Fatalf("status %d body %s, want %d %s %q", rec.Code, rec.Body.String(), c.status, c.code, c.message)
			}
			if e.fs.row != nil || len(e.voiceKeys()) != 0 || e.node.WriteCount() != 0 {
				t.Fatalf("a refusal wrote something: row %+v keys %v writes %d", e.fs.row, e.voiceKeys(), e.node.WriteCount())
			}
		})
	}
}

// The DNS check is counted against the server's OWNER, not the caller: an
// admin enabling voice for an owner already at the running-lookup cap gets
// the 503 at once and starts no lookup (review 5 m1).
func TestHostedOwnDomainVoiceCapIsTheServerOwners(t *testing.T) {
	e, res := newHostedOwnDomain(t)
	release := make(chan struct{})
	res.block = release
	busy := []string{"b1.example.com", "b2.example.com"}
	var wg sync.WaitGroup
	for _, d := range busy {
		// Another server of the same owner: this one's walk must not join them.
		e.mr.Set("route:"+d, `{"target_ip":"mc_srv-8","target_port":25565,"server_uuid":"srv-8"}`)
		e.mr.SAdd("sys:index:routes", d)
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			e.h.d.Pointing.CheckFor(context.Background(), "owner-1", d, services.PurposeVoiceRegion, nil)
		}(d)
	}
	defer func() { close(release); wg.Wait() }()
	// Each running lookup asks CNAME, A and AAAA at once.
	for start := time.Now(); res.count() < 3*len(busy); {
		if time.Since(start) > 2*time.Second {
			t.Fatalf("only %d lookups started", res.count())
		}
		time.Sleep(time.Millisecond)
	}
	before := res.count()
	r := httptest.NewRequest(http.MethodPut, "/api/servers/7/voice/switch", strings.NewReader(`{"enabled":true}`))
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	r = r.WithContext(context.WithValue(r.Context(), "userID", "admin-9"))
	rec := httptest.NewRecorder()
	start := time.Now()
	e.h.SetServerVoice(rec, r)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusServiceUnavailable || out["code"] != "dns_lookup_failed" || time.Since(start) > time.Second {
		t.Fatalf("%d %v after %v, want 503 dns_lookup_failed at once", rec.Code, out, time.Since(start))
	}
	if n := res.count() - before; n != 0 || e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("%d lookups started for a capped owner, row %+v keys %v", n, e.fs.row, e.voiceKeys())
	}
}

// Its region is fixed when the port is allocated: once voice is on, a later
// DNS change neither moves it nor is asked about when it is enabled again, and
// the mod config is not rewritten.
func TestHostedOwnDomainVoiceKeepsItsRegion(t *testing.T) {
	e, res := newHostedOwnDomain(t)
	res.a[odDomain] = addrs(edgeAddr)
	if rec, _ := e.put(t, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("first enable %d %s", rec.Code, rec.Body.String())
	}
	first := *e.fs.row
	e.node.File = e.written()
	writes := e.node.WriteCount()
	e.mr.Set("edge:registry:e2", `{"edge_id":"e2","status":"online","region":"us","wildcard":"*.us.dylaris.com","ip":"`+foreignAddr+`"}`)
	e.mr.Set("voice:registry:r2", `{"voice_id":"r2","region":"us","ip":"203.0.113.99"}`)
	e.mr.SAdd("sys:voices", "r2")
	res.a[odDomain] = addrs(foreignAddr) // now points at us
	e.h.d.Pointing = odChecker(e.h.d.Redis, res)
	before := res.count()
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable %d %s", rec.Code, rec.Body.String())
	}
	if got := *e.fs.row; got.Region != first.Region || got.Port != first.Port || got.Host != ourVoice {
		t.Fatalf("re-enable = %+v, first %+v", got, first)
	}
	if v, _ := out["voice"].(map[string]interface{}); v["host"] != ourVoice {
		t.Fatalf("voice %v", v)
	}
	if n := res.count() - before; n != 0 {
		t.Fatalf("re-enable made %d resolver calls, want 0", n)
	}
	if e.node.WriteCount() != writes {
		t.Fatalf("the mod config was rewritten: %d writes, want %d", e.node.WriteCount(), writes)
	}
}

// A hosted server with voice on a regional address today, which also holds an
// own domain pointing elsewhere: enabling it again answers, writes and
// publishes exactly what it did before, rewrites no mod config and asks no DNS.
func existingHostedCustomerSeesNoChange(t *testing.T) {
	e, res := newHostedOwnDomain(t)
	e.addHostedRoute("a.eu.dylaris.com")
	res.a[odDomain] = addrs(foreignAddr)
	key := strings.Repeat("ab", 32)
	raw, _ := hex.DecodeString(key)
	e.fs.row = &store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Mod: "svc",
		InternalPort: services.VoiceInternalPort, UplinkKey: raw, Host: "a.eu.dylaris.com"}
	e.node.File = handlertest.VoiceFixture(t, "svc_after.properties")

	rec, _ := e.put(t, `{"enabled":true}`)
	if got, want := strings.TrimSpace(rec.Body.String()), `{"restartRequired":true,"success":true,"voice":{"enabled":true,"host":"a.eu.dylaris.com","port":26000,"mod":"svc","allowed":true}}`; got != want {
		t.Fatalf("enable answer\n got %s\nwant %s", got, want)
	}
	if r := e.fs.row; r.Host != "a.eu.dylaris.com" || r.Region != "eu" || r.Port != 26000 {
		t.Fatalf("row %+v", r)
	}
	if n := e.node.WriteCount(); n != 0 {
		t.Fatalf("the mod config was written %d times, want 0", n)
	}
	want := `{"v":1,"region":"eu","port":26000,"internal_port":24454,"key":"` + hex.EncodeToString(e.fs.row.UplinkKey) + `","host":"a.eu.dylaris.com"}`
	if got, _ := e.mr.Get("dylaris:server:srv-7:voice"); got != want {
		t.Fatalf("server key\n got %s\nwant %s", got, want)
	}
	// The reconcile republishes the same key, byte for byte.
	e.mr.Del("dylaris:server:srv-7:voice")
	if err := services.NewVoiceService(e.fs, e.h.d.Redis).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.mr.Get("dylaris:server:srv-7:voice"); got != want {
		t.Fatalf("server key after reconcile\n got %s\nwant %s", got, want)
	}
	if res.count() != 0 {
		t.Fatalf("resolver calls %d, want 0", res.count())
	}
}
