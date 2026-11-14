package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/services/voicecfg"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// --- Managed gateway (remote mode): voice. The fixture is shared with the
// Bedrock tests in gateway_bedrock_remote_test.go. ---

type queuedOp struct {
	op, domain string
	payload    any
}

// remoteExtrasStore adds the remote_routes rows, the outbox and the key
// replacement to the Bedrock fixture's store. The two one-per-server rules are
// enforced like the partial unique indexes.
type remoteExtrasStore struct {
	*bedrockSwitchStore
	mu     sync.Mutex
	routes map[string]store.RemoteRoute
	outbox []queuedOp
	// touchedAt: provider calls already made when domain was touched.
	touchedAt map[string]int
	calls     func() int
	keyWrites int
}

func (f *remoteExtrasStore) GetRemoteRoute(domain string) (*store.RemoteRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.routes[domain]; ok {
		return &r, nil
	}
	return nil, nil
}
func (f *remoteExtrasStore) ListRemoteRoutesByServer(uuid string) ([]store.RemoteRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.RemoteRoute
	for _, r := range f.routes {
		if r.ServerUUID == uuid {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Domain < out[b].Domain })
	return out, nil
}
func (f *remoteExtrasStore) HasOpenOutbox(domain, op string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.outbox {
		if it.domain == domain && (op == "" || it.op == op) {
			return true, nil
		}
	}
	return false, nil
}
func (f *remoteExtrasStore) EnqueueOutbox(op, domain string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outbox = append(f.outbox, queuedOp{op, domain, payload})
	return nil
}
func (f *remoteExtrasStore) TouchRemoteRoute(domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchedAt[domain] = f.calls()
	return nil
}
func (f *remoteExtrasStore) SetRemoteRouteVoice(domain string, port *int, mod string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.routes[domain]
	if !ok {
		return false, nil
	}
	if port != nil {
		for d, o := range f.routes {
			if d != domain && o.ServerUUID == r.ServerUUID && o.VoicePort != nil {
				return false, store.ErrRemoteVoiceTaken
			}
		}
	}
	r.VoicePort, r.VoiceMod = port, mod
	f.routes[domain] = r
	return true, nil
}
func (f *remoteExtrasStore) SetRemoteRouteEdition(domain, edition string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.routes[domain]
	if !ok {
		return false, nil
	}
	if edition != "" {
		for d, o := range f.routes {
			if d != domain && o.ServerUUID == r.ServerUUID && o.Edition != "" {
				return false, store.ErrRemoteBedrockTaken
			}
		}
	}
	r.Edition = edition
	f.routes[domain] = r
	return true, nil
}
func (f *remoteExtrasStore) SetBedrockKey(_ int, key []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyWrites++
	if bytes.Equal(f.key, key) {
		return false, nil
	}
	f.key = append([]byte(nil), key...)
	return true, nil
}
func (f *remoteExtrasStore) GetBedrockKey(int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key, f.key != nil, nil
}

func (f *remoteExtrasStore) row(domain string) store.RemoteRoute {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.routes[domain]
}

func (f *remoteExtrasStore) ops(op string) []queuedOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []queuedOp
	for _, it := range f.outbox {
		if it.op == op {
			out = append(out, it)
		}
	}
	return out
}

// drain stands in for the outbox loop: the queued ops reach the provider.
func (f *remoteExtrasStore) drain(t *testing.T, fake *remotegwtest.Fake) {
	t.Helper()
	f.mu.Lock()
	items := f.outbox
	f.outbox = nil
	f.mu.Unlock()
	c := remotegw.New(fake.URL(), fake.Key)
	for _, it := range items {
		var err error
		switch p := it.payload.(type) {
		case services.OutboxVoice:
			_, err = c.SetVoice(context.Background(), it.domain, p.Enabled, p.TargetPort)
		case services.OutboxBedrock:
			_, err = c.SetBedrock(context.Background(), it.domain, p.Java, p.Bedrock)
		}
		if err != nil {
			t.Fatalf("drain %s %s: %v", it.op, it.domain, err)
		}
	}
}

const (
	remoteAddr  = "a.eu.example.com"
	remoteAddr2 = "b.eu.example.com"
)

type remoteExtrasEnv struct {
	*voiceSwitchEnv
	rs   *remoteExtrasStore
	fake *remotegwtest.Fake
	// base is the provider URL the client uses; a test may put a proxy there.
	base string
}

// newRemoteExtras is the voice and Bedrock fixture on a platform whose gateway
// is the managed one: server srv-7 holds remoteAddr here and on the provider,
// whose region eu (wildcard *.eu.example.com) is online.
func newRemoteExtras(t *testing.T) *remoteExtrasEnv {
	t.Helper()
	e, bs := newBedrockSwitch(t)
	fake := remotegwtest.NewFake(t)
	rs := &remoteExtrasStore{bedrockSwitchStore: bs, touchedAt: map[string]int{},
		routes: map[string]store.RemoteRoute{}, calls: func() int { return len(fake.Calls()) }}
	env := &remoteExtrasEnv{voiceSwitchEnv: e, rs: rs, fake: fake, base: fake.URL()}
	env.addAddress(remoteAddr, false)
	rg := services.NewRemoteGateway(rs, func() *remotegw.Client { return remotegw.New(env.base, fake.Key) })
	e.h.d.Store = rs
	e.h.d.FeatureFlags = services.NewFeatureFlags(rs)
	e.h.d.GatewayRemote = func() (bool, bool) { return true, true }
	e.h.d.RemoteGateway = rg
	return env
}

func (e *remoteExtrasEnv) addAddress(domain string, custom bool) {
	e.rs.routes[domain] = store.RemoteRoute{Domain: domain, ServerUUID: "srv-7", NodeID: 7, Port: 25565, CustomDomain: custom}
	e.fake.AddRoute("link-1", domain, "mc-srv-7", 25565)
}

func (e *remoteExtrasEnv) getVoice(t *testing.T) (*httptest.ResponseRecorder, voiceView) {
	t.Helper()
	r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/servers/7/voice", nil), map[string]string{"id": "7"})
	rec := httptest.NewRecorder()
	e.h.GetServerVoice(rec, r)
	var out struct {
		Voice voiceView `json:"voice"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out.Voice
}

// providerCallIndex is the position of the first provider call with prefix.
func providerCallIndex(fake *remotegwtest.Fake, prefix string) int {
	for i, c := range fake.Calls() {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestRemoteVoiceEnable(t *testing.T) {
	e := newRemoteExtras(t)
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	v, _ := out["voice"].(map[string]interface{})
	if v["enabled"] != true || v["host"] != remoteAddr || v["port"] != float64(30000) || v["mod"] != "svc" || v["allowed"] != true {
		t.Fatalf("voice %v", v)
	}
	if row := e.rs.row(remoteAddr); row.VoicePort == nil || *row.VoicePort != 30000 || row.VoiceMod != "svc" {
		t.Fatalf("column %+v", row)
	}
	if want := voicecfg.SvcEnable(handlertest.VoiceFixture(t, "svc_before.properties"), remoteAddr, 30000); e.written() != want {
		t.Fatalf("config written:\n%s\nwant:\n%s", e.written(), want)
	}
	if e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("local gateway state written: row %+v keys %v", e.fs.row, e.voiceKeys())
	}
	if _, view := e.getVoice(t); !view.Enabled || view.Host != remoteAddr || view.Port != 30000 || view.Mod != "svc" || !view.Allowed {
		t.Fatalf("view %+v", view)
	}
}

func TestRemoteVoiceEnableTouchesTheRowBeforeTheProviderCall(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := e.put(t, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	at, touched := e.rs.touchedAt[remoteAddr]
	put := providerCallIndex(e.fake, "PUT /routes/"+remoteAddr+"/voice")
	if !touched || put < 0 || at > put {
		t.Fatalf("touched %v after %d provider calls, voice put is call %d: the row must be touched before the provider is asked", touched, at, put)
	}
}

func TestRemoteVoiceEnableRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		code   string
		break_ func(e *remoteExtrasEnv)
	}{
		{"suspended", http.StatusForbidden, "", func(e *remoteExtrasEnv) { e.fs.billing = &store.UserBilling{Status: "suspended"} }},
		{"local permission", http.StatusConflict, "voice_not_allowed", func(e *remoteExtrasEnv) { e.fs.global = "" }},
		{"own domain only", http.StatusConflict, "own_domain_unsupported", func(e *remoteExtrasEnv) {
			delete(e.rs.routes, remoteAddr)
			e.addAddress("play.acme.tld", true)
		}},
		{"two labels deep", http.StatusConflict, "no_routes", func(e *remoteExtrasEnv) {
			delete(e.rs.routes, remoteAddr)
			e.addAddress("x.a.eu.example.com", false)
		}},
		{"mod missing", http.StatusConflict, "voice_mod_missing", func(e *remoteExtrasEnv) { e.node.Path = "nothing/here" }},
		{"mod ambiguous", http.StatusConflict, "voice_mod_ambiguous", func(e *remoteExtrasEnv) { e.node.Also = "config/plasmovoice/server/config.toml" }},
		{"provider voice_not_allowed", http.StatusConflict, "voice_not_allowed", func(e *remoteExtrasEnv) { e.fake.SetVoiceRefusal("voice_not_allowed") }},
		{"provider link_without_voice", http.StatusConflict, "link_without_voice", func(e *remoteExtrasEnv) { e.fake.SetVoiceRefusal("link_without_voice") }},
		{"provider no_routes", http.StatusConflict, "no_routes", func(e *remoteExtrasEnv) { e.fake.SetVoiceRefusal("no_routes") }},
		{"provider no_relay", http.StatusConflict, "no_relay", func(e *remoteExtrasEnv) { e.fake.SetVoiceRefusal("no_relay") }},
		{"provider voice_pool_full", http.StatusConflict, "voice_pool_full", func(e *remoteExtrasEnv) { e.fake.SetVoiceRefusal("voice_pool_full") }},
		{"provider kit_retiring", http.StatusConflict, "kit_retiring", func(e *remoteExtrasEnv) { e.fake.SetVoiceRefusal("kit_retiring") }},
		{"provider down", http.StatusBadGateway, "provider_error", func(e *remoteExtrasEnv) { e.fake.FailAfter(1, 1, 503, "unavailable", 0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newRemoteExtras(t)
			c.break_(e)
			rec, out := e.put(t, `{"enabled":true}`)
			if rec.Code != c.status || (c.code != "" && out["code"] != c.code) {
				t.Fatalf("status %d body %s, want %d %s", rec.Code, rec.Body.String(), c.status, c.code)
			}
			for d, r := range e.rs.routes {
				if r.VoicePort != nil {
					t.Fatalf("a refused enable wrote the column of %s", d)
				}
			}
			if e.written() != "" || len(e.rs.outbox) != 0 || len(e.voiceKeys()) != 0 {
				t.Fatalf("a refused enable wrote something: file %q outbox %v keys %v", e.written(), e.rs.outbox, e.voiceKeys())
			}
		})
	}
}

// An OFF still queued would reach the provider after this ON and leave it off
// while the column says on.
func TestRemoteVoiceOnRefusedWhileAnOffIsQueued(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := e.put(t, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("on: %d", rec.Code)
	}
	if rec, _ := e.put(t, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("off: %d", rec.Code)
	}
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusConflict || out["code"] != "change_pending" {
		t.Fatalf("on with an off queued: %d %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(strings.Join(e.fake.Calls(), "\n"), "/voice"); n != 1 {
		t.Fatalf("the refused on reached the provider: %v", e.fake.Calls())
	}
	if e.rs.row(remoteAddr).VoicePort != nil {
		t.Fatal("refused on wrote the column")
	}
	e.rs.drain(t, e.fake)
	if rec, _ := e.put(t, `{"enabled":true}`); rec.Code != http.StatusOK || e.rs.row(remoteAddr).VoicePort == nil {
		t.Fatalf("on after the drain: %d %s", rec.Code, rec.Body.String())
	}
}

// The handler always picks the same address; the index is the guard when the
// server's voice sits on another one.
func TestRemoteVoiceOneAddressPerServer(t *testing.T) {
	e := newRemoteExtras(t)
	e.addAddress(remoteAddr2, false)
	port := 30123
	r := e.rs.routes[remoteAddr2]
	r.VoicePort, r.VoiceMod = &port, "svc"
	e.rs.routes[remoteAddr2] = r
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusConflict || out["code"] != "voice_one_address" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if e.rs.row(remoteAddr).VoicePort != nil || e.written() != "" {
		t.Fatal("second voice address recorded or configured")
	}
}

func TestRemoteVoiceConfigWriteFailureRollsBack(t *testing.T) {
	e := newRemoteExtras(t)
	e.node.FailWrite = true
	rec, _ := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d body %s, want 502", rec.Code, rec.Body.String())
	}
	if e.rs.row(remoteAddr).VoicePort != nil {
		t.Fatal("column not cleared")
	}
	ops := e.rs.ops(services.OutboxOpVoice)
	if len(ops) != 1 || ops[0].domain != remoteAddr || ops[0].payload != (services.OutboxVoice{Enabled: false}) {
		t.Fatalf("queued %+v, want one voice off for %s", ops, remoteAddr)
	}
	if e.node.WriteCount() != 1 || e.written() != "" {
		t.Fatalf("node saw %d writes, written %q; want only the failed enable", e.node.WriteCount(), e.written())
	}
}

func TestRemoteVoiceOffIsNeverRefused(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := e.put(t, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("on: %d", rec.Code)
	}
	enabled := e.written()
	e.node.File = enabled
	e.fs.billing = &store.UserBilling{Status: "suspended"}
	e.fs.global = ""
	e.fs.perms = map[string]bool{"server:7": false}
	e.fake.FailNext(10, 503, "unavailable", 0)
	before := len(e.fake.Calls())
	rec, out := e.put(t, `{"enabled":false}`)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("off: %d %s", rec.Code, rec.Body.String())
	}
	if e.rs.row(remoteAddr).VoicePort != nil {
		t.Fatal("column kept")
	}
	if ops := e.rs.ops(services.OutboxOpVoice); len(ops) != 1 || ops[0].payload != (services.OutboxVoice{Enabled: false}) {
		t.Fatalf("queued %+v", ops)
	}
	if len(e.fake.Calls()) != before {
		t.Fatalf("off called the provider: %v", e.fake.Calls()[before:])
	}
	if want := enabled + voicecfg.SvcDisable(enabled); e.written() != want {
		t.Fatalf("config written back:\n%s\nwant the enable, then:\n%s", e.written(), voicecfg.SvcDisable(enabled))
	}
}

func TestRemoteVoiceUnknownModeAnswers503(t *testing.T) {
	e := newRemoteExtras(t)
	e.h.d.GatewayRemote = func() (bool, bool) { return false, false }
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusServiceUnavailable || out["code"] != "gateway_provider_unknown" {
		t.Fatalf("on: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := e.getVoice(t); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := e.put(t, `{"enabled":false}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("off: %d %s", rec.Code, rec.Body.String())
	}
	if c := e.fake.Calls(); len(c) != 0 {
		t.Fatalf("provider calls %v while the mode is unread", c)
	}
}

// Local mode answers exactly as before the remote branch existed, with a
// managed gateway wired that it must never ask. The bodies were captured from
// the code before that change.
func TestVoiceLocalModeUnchanged(t *testing.T) {
	e := newVoiceSwitch(t)
	fake := remotegwtest.NewFake(t)
	e.h.d.GatewayRemote = func() (bool, bool) { return false, true }
	e.h.d.RemoteGateway = services.NewRemoteGateway(e.fs, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })

	get := func() string {
		r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/servers/7/voice", nil), map[string]string{"id": "7"})
		rec := httptest.NewRecorder()
		e.h.GetServerVoice(rec, r)
		return strings.TrimSpace(rec.Body.String())
	}
	body := func(rec *httptest.ResponseRecorder) string { return strings.TrimSpace(rec.Body.String()) }

	if got, want := get(), `{"success":true,"voice":{"enabled":false,"host":"","port":0,"mod":"","allowed":true}}`; got != want {
		t.Fatalf("get before:\n%s\nwant:\n%s", got, want)
	}
	rec, _ := e.put(t, `{"enabled":true}`)
	if got, want := body(rec), `{"restartRequired":true,"success":true,"voice":{"enabled":true,"host":"a.eu.dylaris.com","port":26000,"mod":"svc","allowed":true}}`; rec.Code != 200 || got != want {
		t.Fatalf("enable %d:\n%s\nwant:\n%s", rec.Code, got, want)
	}
	keys := e.voiceKeys()
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "dylaris:server:srv-7:voice,sys:index:voice:eu,voice:port:eu:26000"; got != want {
		t.Fatalf("voice keys %s, want %s", got, want)
	}
	if e.fs.row == nil || e.fs.row.Port != 26000 || e.fs.row.Region != "eu" || e.fs.row.Host != "a.eu.dylaris.com" {
		t.Fatalf("row %+v", e.fs.row)
	}
	if got, want := get(), `{"success":true,"voice":{"enabled":true,"host":"a.eu.dylaris.com","port":26000,"mod":"svc","allowed":true}}`; got != want {
		t.Fatalf("get after:\n%s\nwant:\n%s", got, want)
	}
	rec, _ = e.put(t, `{"enabled":false}`)
	if got, want := body(rec), `{"restartRequired":true,"success":true,"voice":{"enabled":false,"host":"","port":0,"mod":"","allowed":true}}`; rec.Code != 200 || got != want {
		t.Fatalf("disable %d:\n%s\nwant:\n%s", rec.Code, got, want)
	}
	if e.fs.row != nil || len(e.voiceKeys()) != 0 {
		t.Fatalf("disable left row %+v keys %v", e.fs.row, e.voiceKeys())
	}
	if c := fake.Calls(); len(c) != 0 {
		t.Fatalf("local mode asked the provider: %v", c)
	}
}
