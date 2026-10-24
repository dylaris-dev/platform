package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"

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
	node *voiceNode
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
	return &voiceSwitchEnv{h: &GatewayHandler{state: st}, fs: fs, mr: mr, node: node}
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
	e.node.mu.Lock()
	defer e.node.mu.Unlock()
	return e.node.written.String()
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
	{"voice_mod_missing", func(e *voiceSwitchEnv) { e.node.path = "nothing/here" }},
	{"voice_mod_ambiguous", func(e *voiceSwitchEnv) { e.node.also = "config/plasmovoice/server/config.toml" }},
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
	e.node.path = "nothing/here"
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
	if want := handlers.SvcVoiceEnable(readVoiceFixture(t, "svc_before.properties"), "a.eu.dylaris.com", 26000); e.written() != want {
		t.Fatalf("config written:\n%s\nwant:\n%s", e.written(), want)
	}
}

func TestVoiceNodeWriteFailureRollsBack(t *testing.T) {
	e := newVoiceSwitch(t)
	e.node.failWrite = true
	// A voice_host the owner set by hand, which a write-back would blank.
	e.node.file = strings.Replace(readVoiceFixture(t, "svc_after.properties"), "voice_host=a.eu.dylaris.com:26000", "voice_host=mine.example.org:4000", 1)
	rec, _ := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d body %s, want 502", rec.Code, rec.Body.String())
	}
	if e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("not rolled back: row %+v keys %v", e.fs.row, e.voiceKeys())
	}
	// The failed write changed nothing on the node, so the rollback must not
	// write the disabled config back: that would strip hand-set settings.
	e.node.mu.Lock()
	writes := e.node.writes
	e.node.mu.Unlock()
	if writes != 1 {
		t.Fatalf("node saw %d write requests, want 1 (the enable's own)", writes)
	}
}

func TestVoiceDisableAllowedWhileFlagOff(t *testing.T) {
	e := newVoiceSwitch(t)
	e.fs.global = ""
	e.fs.perms = map[string]bool{"server:7": false}
	e.node.file = readVoiceFixture(t, "svc_after.properties")
	row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Mod: "svc", Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
	e.fs.row = &row
	if err := services.PublishVoice(context.Background(), e.h.state.Redis, row); err != nil {
		t.Fatal(err)
	}
	rec, out := e.put(t, `{"enabled":false}`)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("not disabled: row %+v keys %v", e.fs.row, e.voiceKeys())
	}
	if want := handlers.SvcVoiceDisable(e.node.file); e.written() != want {
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
	if err := services.PublishVoice(context.Background(), e.h.state.Redis, row); err != nil {
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
	h := &GatewayHandler{state: &handlers.AppState{Store: fs}}
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
