package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
	"dylaris-core/store"
)

// --- Managed gateway (remote mode): Bedrock. Fixture in
// gateway_voice_remote_test.go. ---

// keyID names a key in a failure message without its bytes.
func keyID(k []byte) string {
	sum := sha256.Sum256(k)
	return fmt.Sprintf("len %d sha256 %x", len(k), sum[:4])
}

func bedrockBody(domain string, java, bedrock bool) string {
	return fmt.Sprintf(`{"domain":%q,"java":%v,"bedrock":%v}`, domain, java, bedrock)
}

// providerKey is the provider's key for domain, read past the customer Core.
func providerKey(t *testing.T, fake *remotegwtest.Fake, domain string) []byte {
	t.Helper()
	k, err := remotegw.New(fake.URL(), fake.Key).FloodgateKey(context.Background(), domain)
	if err != nil {
		t.Fatalf("provider key of %s: %v", domain, err)
	}
	return k
}

func (e *remoteExtrasEnv) publishedKey() string {
	v, _ := e.mr.Get(services.FloodgateKeyRedisKey("srv-7"))
	return v
}

// assertNoBedrock: the column of every address is empty, no key stored, none
// published.
func (e *remoteExtrasEnv) assertNoBedrock(t *testing.T) {
	t.Helper()
	for d, r := range e.rs.routes {
		if r.Edition != "" {
			t.Fatalf("edition of %s is %q, want Java only", d, r.Edition)
		}
	}
	if e.rs.key != nil || e.mr.Exists(services.FloodgateKeyRedisKey("srv-7")) {
		t.Fatalf("a key was stored (%v) or published (%v)", e.rs.key != nil, e.mr.Exists(services.FloodgateKeyRedisKey("srv-7")))
	}
}

func TestRemoteBedrockAdd(t *testing.T) {
	e := newRemoteExtras(t)
	rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	want := providerKey(t, e.fake, remoteAddr)
	if string(e.rs.key) != string(want) {
		t.Fatalf("stored key %s, provider's %s", keyID(e.rs.key), keyID(want))
	}
	if e.publishedKey() != base64.StdEncoding.EncodeToString(want) {
		t.Fatalf("published key is not the provider's (want %s)", keyID(want))
	}
	if ttl := e.mr.TTL(services.FloodgateKeyRedisKey("srv-7")); ttl != 0 {
		t.Fatalf("TTL %v, want none", ttl)
	}
	if ed := e.rs.row(remoteAddr).Edition; ed != "both" {
		t.Fatalf("edition %q, want both", ed)
	}
	if e.mr.Exists(services.RouteEditionsKey) || len(e.rs.eds) != 0 {
		t.Fatal("local gateway editions written in remote mode")
	}

	// Off, drained, on again: the provider kept its key, so no restart.
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, false), false); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}
	e.rs.drain(t, e.fake)
	rec, out = bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, false, true), false)
	if rec.Code != http.StatusOK || out["restartRequired"] != false {
		t.Fatalf("add again: %d %s", rec.Code, rec.Body.String())
	}
	if ed := e.rs.row(remoteAddr).Edition; ed != "bedrock" {
		t.Fatalf("edition %q, want bedrock", ed)
	}
}

func TestRemoteBedrockTouchesTheRowBeforeTheProviderCall(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	at, touched := e.rs.touchedAt[remoteAddr]
	put := providerCallIndex(e.fake, "PUT /routes/"+remoteAddr+"/bedrock")
	if !touched || put < 0 || at > put {
		t.Fatalf("touched %v after %d provider calls, bedrock put is call %d", touched, at, put)
	}
}

func TestRemoteBedrockOneAddressPerServer(t *testing.T) {
	e := newRemoteExtras(t)
	e.addAddress(remoteAddr2, false)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	calls := len(e.fake.Calls())
	rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr2, true, true), false)
	if rec.Code != http.StatusConflict || out["code"] != "bedrock_one_address" ||
		out["message"] != "With the managed gateway a server has one Bedrock address. Take Bedrock off "+remoteAddr+" first." {
		t.Fatalf("second: %d %s", rec.Code, rec.Body.String())
	}
	if c := e.fake.Calls(); len(c) != calls {
		t.Fatalf("the provider was asked: %v", c[calls:])
	}
	if e.rs.row(remoteAddr2).Edition != "" {
		t.Fatal("second address reserved")
	}
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, false), false); rec.Code != http.StatusOK {
		t.Fatalf("remove first: %d", rec.Code)
	}
	e.rs.drain(t, e.fake)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr2, true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("second after removing the first: %d %s", rec.Code, rec.Body.String())
	}
}

// Two requests for two addresses of one server at once: exactly one wins.
func TestRemoteBedrockRacingAddsOneWins(t *testing.T) {
	for round := 0; round < 20; round++ {
		e := newRemoteExtras(t)
		e.addAddress(remoteAddr2, false)
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i, d := range []string{remoteAddr, remoteAddr2} {
			wg.Add(1)
			go func(i int, d string) {
				defer wg.Done()
				rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(d, true, true), false)
				codes[i] = rec.Code
			}(i, d)
		}
		wg.Wait()
		sort.Ints(codes)
		if codes[0] != http.StatusOK || codes[1] != http.StatusConflict {
			t.Fatalf("round %d: answers %v, want one 200 and one 409", round, codes)
		}
		n := 0
		for _, r := range e.rs.routes {
			if r.Edition != "" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("round %d: %d Bedrock addresses recorded", round, n)
		}
	}
}

// shortKeyProvider serves the fake, except a Floodgate key of 15 bytes.
func shortKeyProvider(t *testing.T, fake *remotegwtest.Fake) string {
	t.Helper()
	u, _ := url.Parse(fake.URL())
	proxy := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/floodgate-key") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"success":true,"domain":"x","key":%q}`, base64.StdEncoding.EncodeToString(make([]byte, 15)))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRemoteBedrockAddRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		admin  bool
		status int
		code   string
		break_ func(e *remoteExtrasEnv)
	}{
		{"audience off", false, http.StatusConflict, "bedrock_disabled", func(e *remoteExtrasEnv) { e.rs.audience = "off" }},
		{"setting read fails", false, http.StatusInternalServerError, "", func(e *remoteExtrasEnv) { e.rs.settingErr = errors.New("pq: connection reset by peer") }},
		{"admin only, caller is not", false, http.StatusConflict, "bedrock_disabled", func(e *remoteExtrasEnv) { e.rs.audience = "admin" }},
		{"suspended", false, http.StatusForbidden, "", func(e *remoteExtrasEnv) { e.fs.billing = &store.UserBilling{Status: "suspended"} }},
		{"own domain", false, http.StatusConflict, "own_domain_unsupported", func(e *remoteExtrasEnv) {
			r := e.rs.routes[remoteAddr]
			r.CustomDomain = true
			e.rs.routes[remoteAddr] = r
		}},
		{"change pending", false, http.StatusConflict, "change_pending", func(e *remoteExtrasEnv) {
			e.rs.outbox = append(e.rs.outbox, queuedOp{services.OutboxOpBedrock, remoteAddr, services.OutboxBedrock{Java: true}})
		}},
		{"provider bedrock_disabled", false, http.StatusConflict, "bedrock_disabled", func(e *remoteExtrasEnv) { e.fake.SetBedrockRefusal("bedrock_disabled") }},
		{"provider bedrock_unavailable", false, http.StatusConflict, "bedrock_unavailable", func(e *remoteExtrasEnv) { e.fake.SetBedrockRefusal("bedrock_unavailable") }},
		{"provider kit_retiring", false, http.StatusConflict, "kit_retiring", func(e *remoteExtrasEnv) { e.fake.SetBedrockRefusal("kit_retiring") }},
		{"key download fails", false, http.StatusBadGateway, "provider_error", func(e *remoteExtrasEnv) { e.fake.FailAfter(1, 1, 500, "internal", 0) }},
		{"key not 16 bytes", false, http.StatusBadGateway, "provider_error", func(e *remoteExtrasEnv) { e.base = shortKeyProvider(t, e.fake) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newRemoteExtras(t)
			c.break_(e)
			rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), c.admin)
			if rec.Code != c.status || (c.code != "" && out["code"] != c.code) {
				t.Fatalf("status %d body %s, want %d %s", rec.Code, rec.Body.String(), c.status, c.code)
			}
			e.assertNoBedrock(t)
		})
	}
}

// A queued OFF must reach the provider before an ON: refused until drained.
func TestRemoteBedrockOnRefusedWhileAnOffIsQueued(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("add: %d", rec.Code)
	}
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, false), false); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}
	calls := len(e.fake.Calls())
	if rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusConflict || out["code"] != "change_pending" {
		t.Fatalf("add with the off queued: %d %s", rec.Code, rec.Body.String())
	}
	if c := e.fake.Calls(); len(c) != calls || e.rs.row(remoteAddr).Edition != "" {
		t.Fatalf("refused add reached the provider (%v) or the column (%q)", c[calls:], e.rs.row(remoteAddr).Edition)
	}
	e.rs.drain(t, e.fake)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusOK || e.rs.row(remoteAddr).Edition != "both" {
		t.Fatalf("add after the drain: %d %s", rec.Code, rec.Body.String())
	}
}

// The provider stores the editions, then answers 502 because its gateway could
// not be told yet. It did store them: the customer side records the same.
func TestRemoteBedrockStoredButUntold(t *testing.T) {
	e := newRemoteExtras(t)
	e.fake.SetBedrockStoredUntold(true)
	rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false)
	if rec.Code != http.StatusOK || out["restartRequired"] != true {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	want := providerKey(t, e.fake, remoteAddr)
	if e.rs.row(remoteAddr).Edition != "both" || string(e.rs.key) != string(want) || e.publishedKey() != base64.StdEncoding.EncodeToString(want) {
		t.Fatalf("edition %q, stored %s, provider %s", e.rs.row(remoteAddr).Edition, keyID(e.rs.key), keyID(want))
	}

	// The same 502 while the provider's list cannot confirm it (its read
	// failure shape says Java only): not recorded, and a retry finishes it.
	e = newRemoteExtras(t)
	e.fake.SetBedrockStoredUntold(true)
	e.fake.SetListSaysOff(true)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusBadGateway {
		t.Fatalf("unconfirmed: %d %s", rec.Code, rec.Body.String())
	}
	e.assertNoBedrock(t)
	e.fake.SetBedrockStoredUntold(false)
	e.fake.SetListSaysOff(false)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusOK || e.rs.row(remoteAddr).Edition != "both" {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRemoteBedrockRemoveIsNeverRefused(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("add: %d", rec.Code)
	}
	key := append([]byte(nil), e.rs.key...)
	e.fs.billing = &store.UserBilling{Status: "suspended"}
	e.rs.audience = "off"
	e.fake.FailNext(10, 503, "unavailable", 0)
	calls := len(e.fake.Calls())
	rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, false), false)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if e.rs.row(remoteAddr).Edition != "" {
		t.Fatal("column kept")
	}
	if ops := e.rs.ops(services.OutboxOpBedrock); len(ops) != 1 || ops[0].payload != (services.OutboxBedrock{Java: true, Bedrock: false}) {
		t.Fatalf("queued %+v", ops)
	}
	if string(e.rs.key) != string(key) {
		t.Fatal("key row changed")
	}
	if c := e.fake.Calls(); len(c) != calls {
		t.Fatalf("remove called the provider: %v", c[calls:])
	}
}

func TestRemoteBedrockViewCarriesNoKey(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("add: %d", rec.Code)
	}
	key := e.rs.key
	rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodGet, "", false)
	body := rec.Body.String()
	for _, enc := range []string{base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key), hex.EncodeToString(key), string(key)} {
		if strings.Contains(body, enc) {
			t.Fatalf("the view carries the key (%s)", keyID(key))
		}
	}
	v, _ := out["bedrock"].(map[string]interface{})
	routes, _ := v["routes"].([]interface{})
	if rec.Code != 200 || v["enabled"] != true || v["active"] != true || v["address"] != remoteAddr || v["available"] != true || len(routes) != 1 {
		t.Fatalf("view %d %v", rec.Code, v)
	}
}

func TestRemoteBedrockViewRefusals(t *testing.T) {
	for _, c := range []struct {
		name, address, refusal string
		break_                 func(e *remoteExtrasEnv)
	}{
		{"qualifying address", remoteAddr, "", func(*remoteExtrasEnv) {}},
		{"own domain only", "", remoteBedrockNoAddress, func(e *remoteExtrasEnv) {
			delete(e.rs.routes, remoteAddr)
			e.addAddress("play.acme.tld", true)
		}},
		{"provider unreachable", "", remoteBedrockUnreachable, func(e *remoteExtrasEnv) { e.fake.FailNext(5, 503, "unavailable", 0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newRemoteExtras(t)
			c.break_(e)
			rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodGet, "", false)
			v, _ := out["bedrock"].(map[string]interface{})
			refusal, _ := v["refusal"].(string)
			if rec.Code != 200 || v["address"] != c.address || refusal != c.refusal || v["available"] != (c.refusal == "") || v["enabled"] != false {
				t.Fatalf("%d %v", rec.Code, v)
			}
		})
	}
}

// The views say which addresses are on an own domain, so the panel can disable
// voice chat and Bedrock there up front.
func TestRemoteViewsFlagOwnDomains(t *testing.T) {
	const own = "play.acme.tld"
	for _, c := range []struct {
		name          string
		setup         func(e *remoteExtrasEnv)
		ownInBedrock  bool
		voiceOwnOnly  bool
		bedrockRoutes int
	}{
		{"provider address only", func(*remoteExtrasEnv) {}, false, false, 0},
		{"own and provider address", func(e *remoteExtrasEnv) { e.addAddress(own, true) }, true, false, 1},
		{"own address only", func(e *remoteExtrasEnv) {
			delete(e.rs.routes, remoteAddr)
			e.addAddress(own, true)
		}, true, true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newRemoteExtras(t)
			c.setup(e)
			_, out := bedrockCall(e.voiceSwitchEnv, http.MethodGet, "", false)
			v, _ := out["bedrock"].(map[string]interface{})
			routes, _ := v["routes"].([]interface{})
			if len(routes) != c.bedrockRoutes {
				t.Fatalf("routes %v", routes)
			}
			for _, r := range routes {
				m, _ := r.(map[string]interface{})
				if (m["ownDomain"] == true) != c.ownInBedrock || m["domain"] != own || m["bedrock"] != false {
					t.Fatalf("route %v", m)
				}
			}
			if _, vv := e.getVoice(t); vv.OwnDomainOnly != c.voiceOwnOnly {
				t.Fatalf("voice view %+v", vv)
			}
			// The OFF answer carries the same view, so the switch does not
			// read as available until the panel reloads.
			rec, off := e.put(t, `{"enabled":false}`)
			ov, _ := off["voice"].(map[string]interface{})
			if rec.Code != http.StatusOK || (ov["ownDomainOnly"] == true) != c.voiceOwnOnly {
				t.Fatalf("off answer %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRemoteBedrockUnknownModeAnswers503(t *testing.T) {
	e := newRemoteExtras(t)
	e.h.d.GatewayRemote = func() (bool, bool) { return false, false }
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		if rec, _ := bedrockCall(e.voiceSwitchEnv, m, bedrockBody(remoteAddr, true, true), false); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", m, rec.Code, rec.Body.String())
		}
	}
	if c := e.fake.Calls(); len(c) != 0 {
		t.Fatalf("provider calls %v", c)
	}
}

// Local mode answers as before the remote branch, with a managed gateway
// wired that it never asks. Bodies captured from the code before the change.
func TestBedrockLocalModeUnchanged(t *testing.T) {
	e, bs := newBedrockSwitch(t)
	fake := remotegwtest.NewFake(t)
	e.h.d.GatewayRemote = func() (bool, bool) { return false, true }
	e.h.d.RemoteGateway = services.NewRemoteGateway(bs, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	body := func(rec *httptest.ResponseRecorder) string { return strings.TrimSpace(rec.Body.String()) }

	rec, _ := bedrockCall(e, http.MethodGet, "", false)
	if got, want := body(rec), `{"bedrock":{"enabled":false,"active":true,"allowed":true,"available":true,"address":"a.eu.dylaris.com","loader":"mods","routes":[]},"success":true}`; got != want {
		t.Fatalf("get before:\n%s\nwant:\n%s", got, want)
	}
	rec, _ = bedrockCall(e, http.MethodPut, edition(true, true), false)
	if got, want := body(rec), `{"route":{"domain":"a.eu.dylaris.com","java":true,"bedrock":true},"success":true}`; got != want {
		t.Fatalf("set:\n%s\nwant:\n%s", got, want)
	}
	if got := e.mr.HGet(services.RouteEditionsKey, "a.eu.dylaris.com"); got != "both" {
		t.Fatalf("edition %q", got)
	}
	if got, _ := e.mr.Get("dylaris:server:srv-7:floodgate"); got == "" || got != base64.StdEncoding.EncodeToString(bs.key) {
		t.Fatalf("published key is not the stored one (%s)", keyID(bs.key))
	}
	rec, _ = bedrockCall(e, http.MethodGet, "", false)
	if got, want := body(rec), `{"bedrock":{"enabled":true,"active":true,"allowed":true,"available":true,"address":"a.eu.dylaris.com","loader":"mods","routes":[{"domain":"a.eu.dylaris.com","java":true,"bedrock":true}]},"success":true}`; got != want {
		t.Fatalf("get after:\n%s\nwant:\n%s", got, want)
	}
	rec, _ = bedrockCall(e, http.MethodPut, edition(true, false), false)
	if got, want := body(rec), `{"route":{"domain":"a.eu.dylaris.com","java":true,"bedrock":false},"success":true}`; got != want {
		t.Fatalf("remove:\n%s\nwant:\n%s", got, want)
	}
	if len(bs.eds) != 0 || e.mr.Exists(services.RouteEditionsKey) {
		t.Fatal("Java only left a row or a field")
	}
	if c := fake.Calls(); len(c) != 0 {
		t.Fatalf("local mode asked the provider: %v", c)
	}
}
