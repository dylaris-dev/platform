package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type bedrockSwitchStore struct {
	*voiceSwitchStore
	key      []byte
	eds      map[string]store.RouteEdition
	audience string
}

func (f *bedrockSwitchStore) EnableBedrock(_ int, k []byte) error {
	if f.key == nil {
		f.key = k
	}
	return nil
}
func (f *bedrockSwitchStore) GetBedrockKey(int) ([]byte, bool, error) {
	return f.key, f.key != nil, nil
}
func (f *bedrockSwitchStore) ListBedrockServerRefs() ([]store.BedrockServerRef, error) {
	if f.key == nil {
		return nil, nil
	}
	return []store.BedrockServerRef{{ServerID: 7, ServerUUID: "srv-7"}}, nil
}
func (f *bedrockSwitchStore) SetRouteEdition(id int, domain string, java, bedrock bool) error {
	if java && !bedrock {
		delete(f.eds, domain)
		return nil
	}
	f.eds[domain] = store.RouteEdition{Domain: domain, ServerID: id, ServerUUID: "srv-7", Java: java, Bedrock: bedrock}
	return nil
}
func (f *bedrockSwitchStore) DeleteRouteEdition(domain string) error {
	delete(f.eds, domain)
	return nil
}
func (f *bedrockSwitchStore) ListRouteEditions(int) ([]store.RouteEdition, error) {
	var out []store.RouteEdition
	for _, e := range f.eds {
		out = append(out, e)
	}
	return out, nil
}
func (f *bedrockSwitchStore) GetSetting(k string) (string, error) {
	if k == services.BedrockSettingKey {
		return f.audience, nil
	}
	return f.voiceSwitchStore.GetSetting(k)
}

// newBedrockSwitch reuses the voice fixture: a gateway-routed server whose
// route sits under the eu edge, plus a live Bedrock entry in eu.
func newBedrockSwitch(t *testing.T) (*voiceSwitchEnv, *bedrockSwitchStore) {
	t.Helper()
	e := newVoiceSwitch(t)
	bs := &bedrockSwitchStore{voiceSwitchStore: e.fs, eds: map[string]store.RouteEdition{}, audience: "all"}
	e.h.state.Store = bs
	e.h.state.FeatureFlags = services.NewFeatureFlags(bs)
	e.mr.Set("bedrock:registry:b1", `{"id":"b1","region":"eu","version":"1"}`)
	e.mr.SAdd("sys:bedrock", "b1")
	return e, bs
}

func bedrockCall(e *voiceSwitchEnv, method, body string, admin bool) (*httptest.ResponseRecorder, map[string]interface{}) {
	r := httptest.NewRequest(method, "/api/servers/7/bedrock", strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", admin))
	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		e.h.GetServerBedrock(rec, r)
	} else {
		e.h.SetRouteEdition(rec, r)
	}
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func edition(java, bedrock bool) string {
	b, _ := json.Marshal(map[string]interface{}{"domain": "a.eu.dylaris.com", "java": java, "bedrock": bedrock})
	return string(b)
}

func TestRouteEditionBothThenGet(t *testing.T) {
	e, bs := newBedrockSwitch(t)
	if rec, _ := bedrockCall(e, http.MethodPut, edition(true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.mr.HGet(services.RouteEditionsKey, "a.eu.dylaris.com"); got != "both" {
		t.Fatalf("edition %q, want both", got)
	}
	got, _ := e.mr.Get("dylaris:server:srv-7:floodgate")
	if got == "" || got != base64.StdEncoding.EncodeToString(bs.key) {
		t.Fatalf("published key %q, stored %x", got, bs.key)
	}
	_, out := bedrockCall(e, http.MethodGet, "", false)
	v, _ := out["bedrock"].(map[string]interface{})
	routes, _ := v["routes"].([]interface{})
	if v["enabled"] != true || v["active"] != true || v["allowed"] != true || v["available"] != true ||
		v["address"] != "a.eu.dylaris.com" || v["loader"] != "mods" || len(routes) != 1 {
		t.Fatalf("view %v", v)
	}

	// Back to Java only: no row, no field.
	if rec, _ := bedrockCall(e, http.MethodPut, edition(true, false), false); rec.Code != http.StatusOK {
		t.Fatalf("java only: %d", rec.Code)
	}
	if len(bs.eds) != 0 || e.mr.Exists(services.RouteEditionsKey) {
		t.Fatal("Java only left a row or a field")
	}
}

func TestRouteEditionRefusals(t *testing.T) {
	for _, c := range []struct {
		name, body, msg string
		admin           bool
		code            int
		break_          func(e *voiceSwitchEnv, bs *bedrockSwitchStore)
	}{
		{"switch off", edition(true, true), bedrockOff, false, http.StatusConflict, func(_ *voiceSwitchEnv, bs *bedrockSwitchStore) { bs.audience = "off" }},
		{"admin only, caller is not", edition(true, true), bedrockOff, false, http.StatusConflict, func(_ *voiceSwitchEnv, bs *bedrockSwitchStore) { bs.audience = "admin" }},
		{"no entry in region", edition(false, true), bedrockNoEntry, false, http.StatusConflict, func(e *voiceSwitchEnv, _ *bedrockSwitchStore) {
			e.mr.Set("bedrock:registry:b1", `{"id":"b1","region":"us","version":"1"}`)
		}},
		{"entry heartbeat expired", edition(true, true), bedrockNoEntry, false, http.StatusConflict, func(e *voiceSwitchEnv, _ *bedrockSwitchStore) { e.mr.Del("bedrock:registry:b1") }},
		{"neither edition", edition(false, false), "A route needs Java, Bedrock or both.", false, http.StatusBadRequest, func(*voiceSwitchEnv, *bedrockSwitchStore) {}},
		{"route of another server", `{"domain":"x.eu.dylaris.com","java":true,"bedrock":true}`, "Route not found for this server", false, http.StatusNotFound, func(*voiceSwitchEnv, *bedrockSwitchStore) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, bs := newBedrockSwitch(t)
			c.break_(e, bs)
			rec, out := bedrockCall(e, http.MethodPut, c.body, c.admin)
			if rec.Code != c.code || out["message"] != c.msg {
				t.Fatalf("status %d body %s, want %d %q", rec.Code, rec.Body.String(), c.code, c.msg)
			}
			if len(bs.eds) != 0 || bs.key != nil || e.mr.Exists(services.RouteEditionsKey) {
				t.Fatal("a refused change wrote something")
			}
		})
	}
}

func TestRouteEditionAdminMayWhileAdminOnly(t *testing.T) {
	e, bs := newBedrockSwitch(t)
	bs.audience = "admin"
	if rec, _ := bedrockCall(e, http.MethodPut, edition(true, true), true); rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
}

// Off pauses Bedrock but never traps an owner: taking Bedrock away and
// changing Java still save, and a Bedrock-only route then admits nobody.
func TestRouteEditionWhileSwitchedOff(t *testing.T) {
	e, bs := newBedrockSwitch(t)
	if rec, _ := bedrockCall(e, http.MethodPut, edition(true, true), false); rec.Code != http.StatusOK {
		t.Fatalf("set: %d", rec.Code)
	}
	bs.audience = "off"
	e.h.state.FeatureFlags.Invalidate(services.BedrockSettingKey)
	if rec, out := bedrockCall(e, http.MethodPut, edition(false, true), false); rec.Code != http.StatusOK {
		t.Fatalf("keep Bedrock, drop Java while off: %d %v", rec.Code, out)
	}
	if got := e.mr.HGet(services.RouteEditionsKey, "a.eu.dylaris.com"); got != "none" {
		t.Fatalf("paused Bedrock-only = %q, want none", got)
	}
	if rec, _ := bedrockCall(e, http.MethodPut, edition(true, false), false); rec.Code != http.StatusOK {
		t.Fatalf("back to Java while off: %d", rec.Code)
	}
}
