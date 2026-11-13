package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/config"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

type providerFakeStore struct {
	store.Store
	settings map[string]string
	audit    []*models.AuditEventIdentity
	// routingMode answers routing_mode while settings has no such key, so the
	// tests' "nothing was stored" checks stay exact; "" reads as never saved.
	routingMode string
	refused     []store.OutboxItem
}

func (f *providerFakeStore) GetSetting(k string) (string, error) {
	if _, ok := f.settings[k]; !ok && k == "routing_mode" && f.routingMode != "" {
		return f.routingMode, nil
	}
	v, ok := f.settings[k]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}
func (f *providerFakeStore) SetSetting(k, v string) error { f.settings[k] = v; return nil }
func (f *providerFakeStore) DeleteSetting(k string) error { delete(f.settings, k); return nil }
func (f *providerFakeStore) CountOpenOutbox() (int, time.Time, error) {
	return 0, time.Time{}, nil
}
func (f *providerFakeStore) ListRefusedOutbox(limit int) ([]store.OutboxItem, error) {
	return f.refused[:min(limit, len(f.refused))], nil
}
func (f *providerFakeStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	f.audit = append(f.audit, ev)
	return nil
}

type providerFixture struct {
	h      *GatewayProviderHandler
	st     *providerFakeStore
	fake   *remotegw.Fake
	resets int
}

// newProviderFixture: env is the GATEWAY_PROVIDER_* config; settings start as given.
func newProviderFixture(t *testing.T, env config.Config, settings map[string]string) *providerFixture {
	t.Helper()
	f := &providerFixture{st: &providerFakeStore{settings: settings, routingMode: "gateway"}, fake: remotegw.NewFake(t)}
	f.h = NewGatewayProviderHandler(Deps{
		Store:                 f.st,
		FeatureFlags:          services.NewFeatureFlags(f.st),
		ResetGatewayRemote:    func() { f.resets++ },
		GatewayProviderConfig: func() (services.ProviderConfig, error) { return services.LoadProviderConfig(f.st, &env) },
	})
	return f
}

func (f *providerFixture) call(t *testing.T, h http.HandlerFunc, method, body string) (int, map[string]interface{}, string) {
	t.Helper()
	r := httptest.NewRequest(method, "/api/settings/gateway/provider", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), "userID", "admin-1"))
	rec := httptest.NewRecorder()
	h(rec, r)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestProviderSettingsNeverReturnTheKey(t *testing.T) {
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	f.st.settings["gateway.provider_url"] = f.fake.URL()
	key := f.fake.Key()

	code, out, raw := f.call(t, f.h.Save, http.MethodPost, fmt.Sprintf(`{"mode":"remote","key":%s}`, jsonStr(key)))
	if code != 200 || out["mode"] != "remote" || out["keySet"] != true || out["account"] == nil {
		t.Fatalf("save: %d %s", code, raw)
	}
	if strings.Contains(raw, key) {
		t.Fatalf("save answer carries the key: %s", raw)
	}
	code, out, raw = f.call(t, f.h.Get, http.MethodGet, "")
	if code != 200 || out["keySet"] != true || out["url"] != f.fake.URL() || strings.Contains(raw, key) {
		t.Fatalf("get: %d %s", code, raw)
	}
	if len(f.st.audit) != 1 || f.st.audit[0].EventType != "gateway_provider_changed" {
		t.Fatalf("audit = %v", f.st.audit)
	}
	if meta, _ := json.Marshal(f.st.audit[0].Metadata); strings.Contains(string(meta), key) {
		t.Fatalf("audit row carries the key: %s", meta)
	}
	if f.resets != 1 {
		t.Fatalf("mode cache resets = %d, want 1", f.resets)
	}
}

func TestProviderSettingsRefuseRemoteWithoutManagedPlan(t *testing.T) {
	managed := remotegw.Account{Product: "managed", Managed: true, Scopes: []string{"gateway.read", "gateway.write"}}
	routeOnly := managed
	routeOnly.Product, routeOnly.Managed = "route_only", false
	suspended := managed
	suspended.Suspended = true
	readOnly := managed
	readOnly.Scopes = []string{"gateway.read"}

	tests := []struct {
		name     string
		acct     *remotegw.Account
		settings map[string]string
		key      string // "" = the fake's key; "-" = send none
		status   int
		code     string
	}{
		{"route-only plan", &routeOnly, nil, "", 400, "not_managed"},
		{"suspended", &suspended, nil, "", 400, "account_suspended"},
		{"no gateway.write", &readOnly, nil, "", 400, "missing_scope"},
		{"wrong key", nil, nil, "dyl_wrong", 400, "unauthorized"},
		{"no key", nil, nil, "-", 400, "no_key"},
		{"byon on", nil, map[string]string{"feature_byon_enabled": "true"}, "", 409, "tenancy_enabled"},
		{"route-only on", nil, map[string]string{"feature_route_only_enabled": "true"}, "", 409, "tenancy_enabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{}
			for k, v := range tc.settings {
				settings[k] = v
			}
			f := newProviderFixture(t, config.Config{}, settings)
			if tc.acct != nil {
				f.fake.SetAccount(*tc.acct)
			}
			body := fmt.Sprintf(`{"mode":"remote","url":%s}`, jsonStr(f.fake.URL()))
			switch tc.key {
			case "":
				body = fmt.Sprintf(`{"mode":"remote","url":%s,"key":%s}`, jsonStr(f.fake.URL()), jsonStr(f.fake.Key()))
			case "-":
			default:
				body = fmt.Sprintf(`{"mode":"remote","url":%s,"key":%s}`, jsonStr(f.fake.URL()), jsonStr(tc.key))
			}
			code, out, raw := f.call(t, f.h.Save, http.MethodPost, body)
			if code != tc.status || out["code"] != tc.code {
				t.Fatalf("%d %s, want %d %s", code, raw, tc.status, tc.code)
			}
			for _, k := range []string{"gateway.provider", "gateway.provider_key", "gateway.provider_url"} {
				if _, stored := f.st.settings[k]; stored {
					t.Fatalf("refused save stored %s", k)
				}
			}
			if f.resets != 0 || len(f.st.audit) != 0 {
				t.Fatalf("refused save reset the cache (%d) or audited (%d)", f.resets, len(f.st.audit))
			}
		})
	}
}

func TestProviderSettingsRefuseKeyWhenEnvSet(t *testing.T) {
	tests := []struct {
		name string
		env  config.Config
		body string
		code string
	}{
		{"key", config.Config{GatewayProviderKey: "env-key"}, `{"mode":"local","key":"other"}`, "key_from_env"},
		{"empty key", config.Config{GatewayProviderKey: "env-key"}, `{"mode":"local","key":""}`, "key_from_env"},
		{"url", config.Config{GatewayProviderURL: "https://env.example"}, `{"mode":"local","url":"https://other.example"}`, "url_from_env"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newProviderFixture(t, tc.env, map[string]string{})
			if code, out, raw := f.call(t, f.h.Save, http.MethodPost, tc.body); code != 409 || out["code"] != tc.code {
				t.Fatalf("%d %s, want 409 %s", code, raw, tc.code)
			}
			if len(f.st.settings) != 0 {
				t.Fatalf("stored %v", f.st.settings)
			}
		})
	}
	// The GET says which fields the environment pins.
	f := newProviderFixture(t, config.Config{GatewayProviderKey: "env-key"}, map[string]string{})
	f.fake.SetKey("env-key")
	if _, out, raw := f.call(t, f.h.Get, http.MethodGet, ""); out["keyFromEnv"] != true || out["urlFromEnv"] != false || strings.Contains(raw, "env-key") {
		t.Fatalf("get: %s", raw)
	}
}

func TestProviderSettingsKeepKeyWhenOmitted(t *testing.T) {
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	f.st.settings["gateway.provider_url"] = f.fake.URL()
	f.st.settings["gateway.provider_key"] = f.fake.Key()

	// Remote with the stored key, then back to local: the key and URL stay,
	// and local mode makes no provider call beyond the view's account read.
	if code, out, raw := f.call(t, f.h.Save, http.MethodPost, `{"mode":"remote"}`); code != 200 || out["mode"] != "remote" {
		t.Fatalf("remote: %d %s", code, raw)
	}
	before := len(f.fake.Calls())
	if code, out, raw := f.call(t, f.h.Save, http.MethodPost, `{"mode":"local"}`); code != 200 || out["mode"] != "local" || out["keySet"] != true {
		t.Fatalf("local: %d %s", code, raw)
	}
	if f.st.settings["gateway.provider_key"] != f.fake.Key() || f.st.settings["gateway.provider_url"] != f.fake.URL() {
		t.Fatalf("settings after switching to local: %v", f.st.settings)
	}
	for _, c := range f.fake.Calls()[before:] {
		if c != "GET /account" {
			t.Fatalf("switch to local called the provider: %v", f.fake.Calls()[before:])
		}
	}

	// Clearing the key deletes the row, so it does not read as unreadable.
	if code, out, raw := f.call(t, f.h.Save, http.MethodPost, `{"mode":"local","key":""}`); code != 200 || out["keySet"] != false || out["keyUnreadable"] != nil {
		t.Fatalf("clear: %d %s", code, raw)
	}
	if _, ok := f.st.settings["gateway.provider_key"]; ok {
		t.Fatal("cleared key row still there")
	}
	// A row that reads as "" is a key GetSetting could not decrypt.
	f.st.settings["gateway.provider_key"] = ""
	if _, out, raw := f.call(t, f.h.Get, http.MethodGet, ""); out["keyUnreadable"] != true || out["mode"] != "local" {
		t.Fatalf("unreadable key: %s", raw)
	}
}

func TestProviderSettingsRefuseBadURL(t *testing.T) {
	for _, u := range []string{"http://gw.example.com", "https://gw.example.com/api", "https://gw.example.com?x=1", "https://user:pw@gw.example.com", "ftp://gw.example.com", "gw.example.com"} {
		f := newProviderFixture(t, config.Config{}, map[string]string{})
		if code, out, raw := f.call(t, f.h.Save, http.MethodPost, fmt.Sprintf(`{"mode":"local","url":%s}`, jsonStr(u))); code != 400 || out["code"] != "invalid_url" {
			t.Errorf("%s: %d %s, want 400 invalid_url", u, code, raw)
		}
	}
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	if code, _, raw := f.call(t, f.h.Save, http.MethodPost, `{"mode":"local","url":"https://gw.example.com/"}`); code != 200 || f.st.settings["gateway.provider_url"] != "https://gw.example.com" {
		t.Fatalf("valid url: %d %s %v", code, raw, f.st.settings)
	}
}

func TestProviderTestConnectionReportsRejectedKey(t *testing.T) {
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	code, out, raw := f.call(t, f.h.Test, http.MethodPost, fmt.Sprintf(`{"url":%s,"key":"dyl_wrong"}`, jsonStr(f.fake.URL())))
	e, _ := out["error"].(map[string]interface{})
	if code != 200 || out["ok"] != false || e["code"] != "unauthorized" || e["message"] != "The provider rejected this key." {
		t.Fatalf("%d %s", code, raw)
	}
	if strings.Contains(raw, "dyl_wrong") || strings.Contains(raw, f.fake.URL()) {
		t.Fatalf("answer carries the key or URL: %s", raw)
	}

	code, out, raw = f.call(t, f.h.Test, http.MethodPost, fmt.Sprintf(`{"url":%s,"key":%s}`, jsonStr(f.fake.URL()), jsonStr(f.fake.Key())))
	if code != 200 || out["ok"] != true || out["account"] == nil {
		t.Fatalf("good key: %d %s", code, raw)
	}
	if len(f.st.settings) != 0 || len(f.st.audit) != 0 {
		t.Fatalf("a test persisted something: %v %v", f.st.settings, f.st.audit)
	}

	f.fake.FailNext(1, http.StatusForbidden, "", 0)
	_, out, _ = f.call(t, f.h.Test, http.MethodPost, fmt.Sprintf(`{"url":%s,"key":%s}`, jsonStr(f.fake.URL()), jsonStr(f.fake.Key())))
	if e, _ := out["error"].(map[string]interface{}); e["code"] != "forbidden" {
		t.Fatalf("403: %v", out)
	}

	// 10 per minute per admin: three used above.
	for i := 0; i < 7; i++ {
		f.call(t, f.h.Test, http.MethodPost, `{}`)
	}
	if code, out, raw := f.call(t, f.h.Test, http.MethodPost, `{}`); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("11th test: %d %s", code, raw)
	}
	// Save shares the bucket.
	if code, out, raw := f.call(t, f.h.Save, http.MethodPost, `{"mode":"local"}`); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("save after 10 tests: %d %s", code, raw)
	}
}

// The managed gateway hands out addresses only in the gateway and both routing
// modes; in ip_port (the install default) every route create is refused, so
// the switch is refused instead of failing later.
func TestProviderSettingsRefuseRemoteInIPPortMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want int
	}{{"", 409}, {"ip_port", 409}, {"gateway", 200}, {"both", 200}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newProviderFixture(t, config.Config{}, map[string]string{})
			f.st.routingMode = tc.mode
			body := fmt.Sprintf(`{"mode":"remote","url":%s,"key":%s}`, jsonStr(f.fake.URL()), jsonStr(f.fake.Key()))
			code, out, raw := f.call(t, f.h.Save, http.MethodPost, body)
			if code != tc.want {
				t.Fatalf("%d %s, want %d", code, raw, tc.want)
			}
			if tc.want == 409 {
				if out["code"] != "routing_mode" || out["message"] != "Switch the routing mode to Gateway or Both first. Servers get their addresses from the gateway only in those modes." {
					t.Fatalf("refusal: %s", raw)
				}
				if len(f.st.settings) != 0 || len(f.fake.Calls()) != 0 {
					t.Fatalf("refused save stored %v or called %v", f.st.settings, f.fake.Calls())
				}
			}
			_, out, raw = f.call(t, f.h.Get, http.MethodGet, "")
			if out["routingModeOk"] != (tc.want == 200) {
				t.Fatalf("get: %s", raw)
			}
		})
	}
	// Local is never refused, whatever the routing mode.
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	f.st.routingMode = "ip_port"
	if code, _, raw := f.call(t, f.h.Save, http.MethodPost, `{"mode":"local"}`); code != 200 {
		t.Fatalf("local: %d %s", code, raw)
	}
}

func TestProviderSettingsListRefusedChanges(t *testing.T) {
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	_, out, raw := f.call(t, f.h.Get, http.MethodGet, "")
	if ob, _ := out["outbox"].(map[string]interface{}); ob == nil || ob["refused"] == nil || len(ob["refused"].([]interface{})) != 0 {
		t.Fatalf("no refusals must be an empty list: %s", raw)
	}
	for i := 0; i < 7; i++ {
		f.st.refused = append(f.st.refused, store.OutboxItem{Op: "proxy", Domain: fmt.Sprintf("d%d.example.com", i),
			LastError: "gateway provider answered 409 proxy_protocol_disabled: Real player IP forwarding is not enabled on this platform."})
	}
	_, out, raw = f.call(t, f.h.Get, http.MethodGet, "")
	ob, _ := out["outbox"].(map[string]interface{})
	list, _ := ob["refused"].([]interface{})
	if len(list) != 5 {
		t.Fatalf("refused = %s, want 5 items", raw)
	}
	first, _ := list[0].(map[string]interface{})
	if first["op"] != "proxy" || first["domain"] != "d0.example.com" || !strings.Contains(first["error"].(string), "proxy_protocol_disabled") || len(first) != 3 {
		t.Fatalf("item = %v", first)
	}
}
