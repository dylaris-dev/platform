package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type bedrockSwitchStore struct {
	*voiceSwitchStore
	key []byte
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
func (f *bedrockSwitchStore) DisableBedrock(int) error { f.key = nil; return nil }
func (f *bedrockSwitchStore) ListBedrockServers() ([]store.BedrockServer, error) {
	return nil, nil
}

// newBedrockSwitch reuses the voice fixture: a gateway-routed server whose
// route sits under the eu edge, plus a live Bedrock entry in eu.
func newBedrockSwitch(t *testing.T) (*voiceSwitchEnv, *bedrockSwitchStore) {
	t.Helper()
	e := newVoiceSwitch(t)
	bs := &bedrockSwitchStore{voiceSwitchStore: e.fs}
	e.h.state.Store = bs
	e.mr.Set("bedrock:registry:b1", `{"id":"b1","region":"eu","version":"1"}`)
	e.mr.SAdd("sys:bedrock", "b1")
	return e, bs
}

func bedrockCall(e *voiceSwitchEnv, method, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	r := httptest.NewRequest(method, "/api/servers/7/bedrock", strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		e.h.GetServerBedrock(rec, r)
	} else {
		e.h.SetServerBedrock(rec, r)
	}
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestBedrockEnableThenGet(t *testing.T) {
	e, bs := newBedrockSwitch(t)
	if rec, _ := bedrockCall(e, http.MethodPut, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := e.mr.Get("dylaris:server:srv-7:floodgate")
	if got != base64.StdEncoding.EncodeToString(bs.key) {
		t.Fatalf("published %q, stored %x", got, bs.key)
	}
	_, out := bedrockCall(e, http.MethodGet, "")
	v, _ := out["bedrock"].(map[string]interface{})
	if v["enabled"] != true || v["available"] != true || v["address"] != "a.eu.dylaris.com" || v["loader"] != "mods" {
		t.Fatalf("view %v", v)
	}
	if rec, _ := bedrockCall(e, http.MethodPut, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d", rec.Code)
	}
	if bs.key != nil || e.mr.Exists("dylaris:server:srv-7:floodgate") {
		t.Fatal("disable left the row or key")
	}
}

func TestBedrockEnableRefusedWhenUnavailable(t *testing.T) {
	for _, c := range []struct {
		name, msg string
		break_    func(e *voiceSwitchEnv)
	}{
		{"no entry in region", bedrockNoEntry, func(e *voiceSwitchEnv) {
			e.mr.Set("bedrock:registry:b1", `{"id":"b1","region":"us","version":"1"}`)
		}},
		{"entry heartbeat expired", bedrockNoEntry, func(e *voiceSwitchEnv) { e.mr.Del("bedrock:registry:b1") }},
		{"not routed", bedrockNotRouted, func(e *voiceSwitchEnv) { e.mr.Del("route:a.eu.dylaris.com") }},
		{"gateway off", bedrockNotRouted, func(e *voiceSwitchEnv) { e.fs.routing = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, bs := newBedrockSwitch(t)
			c.break_(e)
			rec, out := bedrockCall(e, http.MethodPut, `{"enabled":true}`)
			if rec.Code != http.StatusConflict || out["message"] != c.msg {
				t.Fatalf("status %d body %s, want 409 %q", rec.Code, rec.Body.String(), c.msg)
			}
			if bs.key != nil || e.mr.Exists("dylaris:server:srv-7:floodgate") {
				t.Fatal("a refused enable wrote something")
			}
		})
	}
}
