package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/services/voicecfg"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

const wlName = "voice.play.example.com"

func (f *voiceSwitchStore) SetVoicePublicHost(_ int, host, state string) error {
	if f.row != nil {
		f.row.PublicHost, f.row.PublicState = host, state
	}
	return nil
}
func (f *voiceSwitchStore) RecordAddressDNSCheck(name, status, region string, ok bool) (store.AddressDNSCheck, error) {
	if f.dns == nil {
		f.dns = map[string]store.AddressDNSCheck{}
	}
	c := f.dns[name]
	c.Name, c.Status, c.Region = name, status, region
	if ok {
		c.Misses = 0
	} else {
		c.Misses++
	}
	f.dns[name] = c
	return c, nil
}
func (f *voiceSwitchStore) GetAddressDNSChecks(names []string) (map[string]store.AddressDNSCheck, error) {
	out := map[string]store.AddressDNSCheck{}
	for _, n := range names {
		if c, ok := f.dns[strings.ToLower(n)]; ok {
			out[strings.ToLower(n)] = c
		}
	}
	return out, nil
}

func (m *managedStore) SetLinkRouteVoicePublicHost(domain, host, state string) error {
	if v, ok := m.voice[domain]; ok {
		v.PublicHost, v.PublicState = host, state
		m.voice[domain] = v
	}
	return nil
}

func TestValidVoiceName(t *testing.T) {
	hosters := []services.HosterDomain{{Domain: "dylaris.com"}, {Domain: "eu.dylaris.com"}}
	routes := []string{"play.example.com", "a.eu.dylaris.com"}
	for _, c := range []struct {
		name, in string
		routes   []string
		want     string
		ok       bool
	}{
		{"equal", "play.example.com", routes, "play.example.com", true},
		{"under", wlName, routes, wlName, true},
		{"deeper", "a.b.play.example.com", routes, "a.b.play.example.com", true},
		{"uppercase is lowered", "VOICE.Play.Example.COM", routes, wlName, true},
		{"trailing dot", wlName + ".", routes, wlName, true},
		{"unrelated domain", "voice.other.org", routes, "voice.other.org", false},
		{"label boundary", "notplay.example.com", routes, "notplay.example.com", false},
		{"under a hoster domain", "voice.a.eu.dylaris.com", routes, "voice.a.eu.dylaris.com", false},
		{"a hoster domain route itself", "dylaris.com", []string{"dylaris.com"}, "dylaris.com", false},
		{"an IP", "203.0.113.5", []string{"113.5"}, "203.0.113.5", false},
		{"too many labels", strings.Repeat("a.", 130) + "play.example.com", routes, "", false},
		{"bad label", "-x-.play.example.com", routes, "-x-.play.example.com", false},
		{"one label", "localhost", []string{"localhost"}, "localhost", false},
		{"no routes", wlName, nil, wlName, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := validVoiceName(c.in, c.routes, hosters)
			if ok != c.ok || (c.want != "" && got != c.want) {
				t.Fatalf("validVoiceName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

// newVoiceNameEnv is a server whose only address is odDomain with voice on in
// eu (our name ourVoice, port 26000) and the svc config on its node.
func newVoiceNameEnv(t *testing.T) (*voiceSwitchEnv, *odResolver) {
	t.Helper()
	e, res := newHostedOwnDomain(t)
	e.fs.row = &store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Mod: "svc",
		InternalPort: services.VoiceInternalPort, UplinkKey: make([]byte, 32), Host: ourVoice}
	e.node.File = voicecfg.SvcEnable(handlertest.VoiceFixture(t, "svc_before.properties"), ourVoice, 26000)
	return e, res
}

func (e *voiceSwitchEnv) putName(t *testing.T, name string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	r := httptest.NewRequest(http.MethodPut, "/api/servers/7/voice/name", strings.NewReader(string(body)))
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	r = r.WithContext(context.WithValue(r.Context(), "userID", "owner-1"))
	rec := httptest.NewRecorder()
	e.h.SetServerVoiceName(rec, r)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestVoiceNameStates(t *testing.T) {
	e, res := newVoiceNameEnv(t)
	base := e.node.File

	// No record yet: pending, the mod keeps our name.
	rec, out := e.putName(t, wlName)
	if want := wlName + " does not resolve yet. Add a CNAME from " + wlName + " to " + ourVoice + ", wait a few minutes, then try again."; rec.Code != http.StatusConflict ||
		out["code"] != "voice_host_not_pointing" || out["message"] != want {
		t.Fatalf("pending: %d %v, want 409 %q", rec.Code, out, want)
	}
	if r := e.fs.row; r.PublicHost != wlName || r.PublicState != store.VoicePublicPending || e.node.WriteCount() != 0 {
		t.Fatalf("pending: row %+v writes %d", r, e.node.WriteCount())
	}
	if c := e.fs.dns[wlName]; c.Status != "not_found" || c.Misses != 1 {
		t.Fatalf("stored check %+v", c)
	}

	// The record is created; Check again is the same call.
	res.cname[wlName] = ourVoice
	e.h.d.Pointing = odChecker(e.h.d.Redis, res)
	rec, out = e.putName(t, wlName)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("active: %d %s", rec.Code, rec.Body.String())
	}
	v, _ := out["voice"].(map[string]interface{})
	if v["host"] != wlName || v["modHost"] != wlName || v["publicHost"] != wlName || v["publicState"] != "active" || v["ourHost"] != ourVoice {
		t.Fatalf("active view %v", v)
	}
	if want := voicecfg.SvcEnable(base, wlName, 26000); e.written() != want || e.fs.row.PublicState != store.VoicePublicActive {
		t.Fatalf("active: config\n%s\nwant\n%s\nrow %+v", e.written(), want, e.fs.row)
	}
	if c := e.fs.dns[wlName]; c.Status != "ok" || c.Region != "eu" || c.Misses != 0 {
		t.Fatalf("stored check %+v", c)
	}
	if key, _ := e.mr.Get("dylaris:server:srv-7:voice"); strings.Contains(key, wlName) {
		t.Fatalf("the relay key names the customer's name: %s", key)
	}

	// The DNS fails: nothing is stored, the config is not touched.
	e.node.File = e.written()
	writes := e.node.WriteCount()
	res.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	e.h.d.Pointing = odChecker(e.h.d.Redis, res)
	rec, out = e.putName(t, wlName)
	if rec.Code != http.StatusServiceUnavailable || out["code"] != "dns_lookup_failed" {
		t.Fatalf("lookup failed: %d %v", rec.Code, out)
	}
	if r := e.fs.row; r.PublicState != store.VoicePublicActive || e.node.WriteCount() != writes || e.fs.dns[wlName].Misses != 0 {
		t.Fatalf("lookup failed changed something: row %+v writes %d check %+v", r, e.node.WriteCount(), e.fs.dns[wlName])
	}
	res.fail = nil

	// Cancel: our name back into the mod. The node keeps every write.
	done := len(e.written())
	rec, out = e.putName(t, "")
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if v, _ := out["voice"].(map[string]interface{}); v["host"] != ourVoice || v["publicState"] != nil {
		t.Fatalf("cancel view %v", v)
	}
	if want := voicecfg.SvcEnable(e.node.File, ourVoice, 26000); e.written()[done:] != want || want != base || e.fs.row.PublicHost != "" || e.fs.row.PublicState != "" {
		t.Fatalf("cancel: config\n%s\nrow %+v", e.written()[done:], e.fs.row)
	}
}

func TestVoiceNameWrongRegion(t *testing.T) {
	e, res := newVoiceNameEnv(t)
	e.mr.Set("edge:registry:e2", `{"edge_id":"e2","status":"online","region":"us","wildcard":"*.us.dylaris.com","ip":"`+foreignAddr+`"}`)
	res.cname[wlName] = "voice.us.dylaris.com"
	// The same name's row as the Bedrock check left it (review I1).
	e.fs.dns = map[string]store.AddressDNSCheck{wlName: {Name: wlName, Status: "ok", Region: "us"}}
	rec, out := e.putName(t, wlName)
	want := wlName + " points at the gateway's region us, but this server's voice chat runs in eu. Point it at " + ourVoice + "."
	if rec.Code != http.StatusConflict || out["code"] != "voice_host_not_pointing" || out["message"] != want {
		t.Fatalf("%d %v, want 409 %q", rec.Code, out, want)
	}
	if r := e.fs.row; r.PublicState != store.VoicePublicPending || e.node.WriteCount() != 0 {
		t.Fatalf("row %+v writes %d", r, e.node.WriteCount())
	}
	// The shared row keeps the checker's answer: ok in us, no miss counted.
	if c := e.fs.dns[wlName]; c.Status != "ok" || c.Region != "us" || c.Misses != 0 {
		t.Fatalf("stored check %+v, want ok in us with no miss", c)
	}
}

// A fallen back name that still misses keeps its state, so the restart
// warning stays; a working one goes active again.
func TestVoiceNameCheckAgainAfterFallBack(t *testing.T) {
	e, res := newVoiceNameEnv(t)
	e.fs.row.PublicHost, e.fs.row.PublicState = wlName, store.VoicePublicFallenBack
	if rec, _ := e.putName(t, wlName); rec.Code != http.StatusConflict || e.fs.row.PublicState != store.VoicePublicFallenBack {
		t.Fatalf("%d row %+v", rec.Code, e.fs.row)
	}
	res.cname[wlName] = ourVoice
	e.h.d.Pointing = odChecker(e.h.d.Redis, res)
	if rec, _ := e.putName(t, wlName); rec.Code != http.StatusOK || e.fs.row.PublicState != store.VoicePublicActive {
		t.Fatalf("%d row %+v", rec.Code, e.fs.row)
	}
}

func TestVoiceNameRateLimited(t *testing.T) {
	e, res := newVoiceNameEnv(t)
	e.h.d.PointingLimiter = httpx.NewIPRateLimiter()
	for i := 0; i < pointingChecksPerMin; i++ {
		e.h.d.PointingLimiter.Allow("user:owner-1", pointingChecksPerMin)
	}
	rec, out := e.putName(t, wlName)
	if rec.Code != http.StatusTooManyRequests || out["code"] != "rate_limited" || res.count() != 0 || e.fs.row.PublicState != "" {
		t.Fatalf("%d %v calls %d row %+v", rec.Code, out, res.count(), e.fs.row)
	}
}

func TestVoiceNameNeedsVoiceOn(t *testing.T) {
	e, res := newHostedOwnDomain(t)
	res.cname[wlName] = ourVoice
	rec, out := e.putName(t, wlName)
	if rec.Code != http.StatusConflict || out["code"] != "voice_off" || out["message"] != "Turn voice chat on first." {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if res.count() != 0 || e.node.WriteCount() != 0 {
		t.Fatalf("calls %d writes %d", res.count(), e.node.WriteCount())
	}
}

// Review Focus 4: a name that is not the caller's own route or under it is
// refused before anything is resolved.
func TestVoiceNameAdmissionZeroLookups(t *testing.T) {
	for _, name := range []string{
		"voice.other.org",        // no route at all
		"voice.b.example.com",    // another server's route
		"voice.x.eu.dylaris.com", // under the platform's domain
		"203.0.113.10",           // an IP
		"notplay.example.com",    // not under at a label boundary
		"bad name.play.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			e, res := newVoiceNameEnv(t)
			e.mr.Set("route:b.example.com", `{"target_ip":"mc_srv-8","target_port":25565,"server_uuid":"srv-8"}`)
			e.mr.SAdd("sys:index:routes", "b.example.com")
			res.a[name] = addrs(edgeAddr)
			rec, out := e.putName(t, name)
			if rec.Code != http.StatusBadRequest || out["code"] != "voice_host_invalid" {
				t.Fatalf("%d %v, want 400 voice_host_invalid", rec.Code, out)
			}
			if res.count() != 0 || e.fs.row.PublicState != "" || e.node.WriteCount() != 0 {
				t.Fatalf("calls %d row %+v writes %d", res.count(), e.fs.row, e.node.WriteCount())
			}
		})
	}
	e, _ := newVoiceNameEnv(t)
	if _, out := e.putName(t, "voice.other.org"); out["message"] != "voice.other.org has to be "+odDomain+" or a name under it." {
		t.Fatalf("sentence %v", out["message"])
	}

	t.Run("route-only", func(t *testing.T) {
		m, res := newOwnDomainEnv(t)
		res.a[odDomain] = addrs(edgeAddr)
		res.a["voice.other.org"] = addrs(edgeAddr)
		rec := linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, odDomain, `{"enabled":true,"public_host":"voice.other.org"}`, "owner-1", false)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["code"] != "voice_host_invalid" || out["message"] != "voice.other.org has to be "+odDomain+" or a name under it." {
			t.Fatalf("%d %v", rec.Code, out)
		}
		if res.count() != 0 || len(m.ms.voice) != 0 {
			t.Fatalf("calls %d rows %v", res.count(), m.ms.voice)
		}
	})

	t.Run("route-only on our domain", func(t *testing.T) {
		m, res := newOwnDomainEnv(t)
		rec := linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, linkDomain, `{"enabled":true,"public_host":"voice.`+linkDomain+`"}`, "owner-1", false)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["code"] != "voice_host_invalid" || out["message"] != "Add an address on your own domain first." || res.count() != 0 {
			t.Fatalf("%d %v calls %d", rec.Code, out, res.count())
		}
	})
}

func (m *managedEnv) putRouteVoice(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	rec := linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, odDomain, body, "owner-1", false)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// Route-only: mod_host follows the state, host stays our name, and an absent
// public_host leaves the name alone.
func TestLinkRouteVoicePublicHost(t *testing.T) {
	m, res := newOwnDomainEnv(t)
	res.a[odDomain] = addrs(edgeAddr)

	rec, out := m.putRouteVoice(t, `{"enabled":true,"public_host":"`+wlName+`"}`)
	if rec.Code != http.StatusConflict || out["code"] != "voice_host_not_pointing" {
		t.Fatalf("pending: %d %v", rec.Code, out)
	}
	if v := m.ms.voice[odDomain]; v.PublicHost != wlName || v.PublicState != store.VoicePublicPending || v.Host != ourVoice || v.ModHost() != ourVoice {
		t.Fatalf("pending row %+v; voice must be on with our name", v)
	}

	res.cname[wlName] = ourVoice
	m.e.h.d.Pointing = odChecker(m.e.h.d.Redis, res)
	rec, out = m.putRouteVoice(t, `{"enabled":true,"public_host":"`+wlName+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("active: %d %s", rec.Code, rec.Body.String())
	}
	v, _ := out["voice"].(map[string]interface{})
	if v["host"] != ourVoice || v["mod_host"] != wlName || v["public_host"] != wlName || v["public_state"] != "active" {
		t.Fatalf("active view %v", v)
	}

	// The sweep's re-assert sends no public_host: the name stays.
	before := res.count()
	rec, out = m.putRouteVoice(t, `{"enabled":true}`)
	if v, _ := out["voice"].(map[string]interface{}); rec.Code != http.StatusOK || v["mod_host"] != wlName || res.count() != before {
		t.Fatalf("re-assert: %d %v calls %d", rec.Code, out, res.count()-before)
	}

	rec, out = m.putRouteVoice(t, `{"enabled":true,"public_host":""}`)
	if v, _ := out["voice"].(map[string]interface{}); rec.Code != http.StatusOK || v["mod_host"] != ourVoice || v["public_state"] != "" || m.ms.voice[odDomain].PublicHost != "" {
		t.Fatalf("cleared: %d %v row %+v", rec.Code, out, m.ms.voice[odDomain])
	}
}

// The link is told OUR name in domain whatever the white-label state.
func TestLinkVoiceIgnoresTheWhiteLabelName(t *testing.T) {
	h, fs, key, token := routeOnlyKit(t)
	fs.voice = []store.LinkRouteVoice{{Domain: odDomain, OwnerID: "owner-1", LinkToken: token, TargetHost: "127.0.0.1", Port: 26002,
		TargetPort: 24455, UplinkKey: make([]byte, 32), Host: ourVoice, PublicHost: wlName, PublicState: store.VoicePublicActive}}
	rec := callAs(h.LinkVoice, http.MethodGet, "/api/warp/link/voice", "", key)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"domain":"`+ourVoice+`"`) || strings.Contains(rec.Body.String(), wlName) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// An unread gateway mode answers 503: it cannot tell whose voice the name is
// for. The managed gateway itself is gateway_voice_name_remote_test.go.
func TestVoiceNameUnreadModeAnswers503(t *testing.T) {
	e, _ := newVoiceNameEnv(t)
	e.h.d.GatewayRemote = func() (bool, bool) { return false, false }
	if rec, out := e.putName(t, wlName); rec.Code != http.StatusServiceUnavailable || out["code"] != "gateway_provider_unknown" {
		t.Fatalf("unread mode: %d %v", rec.Code, out)
	}
}

// Turning voice on again keeps an active name in the mod, and the relays our
// name.
func TestVoiceReenableKeepsTheActiveName(t *testing.T) {
	e, _ := newVoiceNameEnv(t)
	e.fs.row.PublicHost, e.fs.row.PublicState = wlName, store.VoicePublicActive
	rec, out := e.put(t, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if v, _ := out["voice"].(map[string]interface{}); v["host"] != wlName || v["ourHost"] != ourVoice {
		t.Fatalf("view %v", v)
	}
	if got := e.written(); !strings.Contains(got, "voice_host="+wlName+":26000") {
		t.Fatalf("config %s", got)
	}
	if key, _ := e.mr.Get("dylaris:server:srv-7:voice"); !strings.Contains(key, `"host":"`+ourVoice+`"`) {
		t.Fatalf("relay key %s", key)
	}
}

// Review I2: while a name is active, a miss on Check again of it, or a
// different name that does not point yet, keeps the active name in the row
// and the mod, and only answers the sentence. A different name that points
// replaces it.
func TestVoiceNameActiveSurvivesAUserMiss(t *testing.T) {
	const other = "mc.play.example.com"
	for _, c := range []struct {
		name, send string
		dns        func(r *odResolver)
	}{
		{"same name, record gone", wlName, func(*odResolver) {}},
		{"same name, other region", wlName, func(r *odResolver) { r.cname[wlName] = "voice.us.dylaris.com" }},
		{"different name, not pointing", other, func(*odResolver) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, res := newVoiceNameEnv(t)
			e.mr.Set("edge:registry:e2", `{"edge_id":"e2","status":"online","region":"us","wildcard":"*.us.dylaris.com","ip":"`+foreignAddr+`"}`)
			e.fs.row.PublicHost, e.fs.row.PublicState = wlName, store.VoicePublicActive
			c.dns(res)
			rec, out := e.putName(t, c.send)
			if rec.Code != http.StatusConflict || out["code"] != "voice_host_not_pointing" {
				t.Fatalf("%d %v, want 409 voice_host_not_pointing", rec.Code, out)
			}
			if r := e.fs.row; r.PublicHost != wlName || r.PublicState != store.VoicePublicActive || e.node.WriteCount() != 0 {
				t.Fatalf("row %+v writes %d; the active name must stay", r, e.node.WriteCount())
			}
		})
	}
	t.Run("different name, pointing", func(t *testing.T) {
		e, res := newVoiceNameEnv(t)
		e.fs.row.PublicHost, e.fs.row.PublicState = wlName, store.VoicePublicActive
		res.cname[other] = ourVoice
		if rec, _ := e.putName(t, other); rec.Code != http.StatusOK || e.fs.row.PublicHost != other || e.fs.row.PublicState != store.VoicePublicActive ||
			!strings.Contains(e.written(), "voice_host="+other+":26000") {
			t.Fatalf("%d row %+v config %s", rec.Code, e.fs.row, e.written())
		}
	})
}

func TestLinkRouteVoiceActiveSurvivesAUserMiss(t *testing.T) {
	for _, send := range []string{wlName, "mc.play.example.com"} {
		t.Run(send, func(t *testing.T) {
			m, res := newOwnDomainEnv(t)
			res.a[odDomain] = addrs(edgeAddr)
			res.cname[wlName] = ourVoice
			if rec, _ := m.putRouteVoice(t, `{"enabled":true,"public_host":"`+wlName+`"}`); rec.Code != http.StatusOK {
				t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
			}
			delete(res.cname, wlName)
			m.e.h.d.Pointing = odChecker(m.e.h.d.Redis, res)
			rec, out := m.putRouteVoice(t, `{"enabled":true,"public_host":"`+send+`"}`)
			if rec.Code != http.StatusConflict || out["code"] != "voice_host_not_pointing" {
				t.Fatalf("%d %v", rec.Code, out)
			}
			if v := m.ms.voice[odDomain]; v.PublicHost != wlName || v.PublicState != store.VoicePublicActive || v.ModHost() != wlName {
				t.Fatalf("row %+v; the active name must stay", v)
			}
		})
	}
}
