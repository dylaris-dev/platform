package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/config"
	"dylaris-core/handlers/health"
	"dylaris-core/services"
	"dylaris-core/services/remotegw/remotegwtest"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A stored or env key only ever goes to the URL in force: another address needs
// the key typed in, and nothing is sent anywhere before that.
func TestProviderKeyOnlyGoesToTheAddressInForce(t *testing.T) {
	envKey := func(f *remotegwtest.Fake) config.Config { return config.Config{GatewayProviderKey: f.Key()} }
	tests := []struct {
		name   string
		env    func(*remotegwtest.Fake) config.Config
		test   bool // the test endpoint, else Save
		status int
		code   string
	}{
		{"test, stored key", nil, true, 400, "no_key"},
		{"test, env key", envKey, true, 409, "key_from_env"},
		{"save, stored key", nil, false, 400, "no_key"},
		{"save, env key", envKey, false, 409, "key_from_env"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inForce := remotegwtest.NewFake(t)
			env := config.Config{}
			if tc.env != nil {
				env = tc.env(inForce)
			}
			f := newProviderFixture(t, env, map[string]string{"gateway.provider_url": inForce.URL()})
			if tc.env == nil {
				f.st.settings["gateway.provider_key"] = inForce.Key()
			}
			other := remotegwtest.NewFake(t)
			h := f.h.Save
			body := fmt.Sprintf(`{"mode":"remote","url":%s}`, jsonStr(other.URL()))
			if tc.test {
				h = f.h.Test
				body = fmt.Sprintf(`{"url":%s}`, jsonStr(other.URL()))
			}
			code, out, raw := f.call(t, h, http.MethodPost, body)
			if code != tc.status || out["code"] != tc.code {
				t.Fatalf("%d %s, want %d %s", code, raw, tc.status, tc.code)
			}
			if len(other.Calls()) != 0 || len(inForce.Calls()) != 0 {
				t.Fatalf("provider calls: new address %v, address in force %v", other.Calls(), inForce.Calls())
			}
			if f.st.settings["gateway.provider_url"] != inForce.URL() || f.st.settings["gateway.provider"] != "" {
				t.Fatalf("settings changed: %v", f.st.settings)
			}
		})
	}

	// With the key typed in, the new address is fine.
	f := newProviderFixture(t, config.Config{}, map[string]string{"gateway.provider_key": "old-key"})
	body := fmt.Sprintf(`{"url":%s,"key":%s}`, jsonStr(f.fake.URL()), jsonStr(f.fake.Key()))
	if code, out, raw := f.call(t, f.h.Test, http.MethodPost, body); code != 200 || out["ok"] != true {
		t.Fatalf("test with key: %d %s", code, raw)
	}
	body = fmt.Sprintf(`{"mode":"local","url":%s,"key":%s}`, jsonStr(f.fake.URL()), jsonStr(f.fake.Key()))
	if code, _, raw := f.call(t, f.h.Save, http.MethodPost, body); code != 200 || f.st.settings["gateway.provider_key"] != f.fake.Key() {
		t.Fatalf("save with key: %d %s", code, raw)
	}
}

func TestProviderSettingsGetIsCachedAndQuiet(t *testing.T) {
	// No key: no outbound call at all.
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	f.st.settings["gateway.provider_url"] = f.fake.URL()
	f.call(t, f.h.Get, http.MethodGet, "")
	if n := len(f.fake.Calls()); n != 0 {
		t.Fatalf("GET without a key called the provider %d times", n)
	}

	f.st.settings["gateway.provider_key"] = f.fake.Key()
	f.call(t, f.h.Get, http.MethodGet, "")
	if _, out, raw := f.call(t, f.h.Get, http.MethodGet, ""); out["account"] == nil {
		t.Fatalf("cached GET: %s", raw)
	}
	if n := len(f.fake.Calls()); n != 1 {
		t.Fatalf("two GETs made %d provider calls, want 1", n)
	}

	// A failure is kept too, for a shorter time.
	clock := time.Now()
	f.h.now = func() time.Time { return clock }
	f.h.dropAccountCache()
	f.fake.FailNext(1, http.StatusUnauthorized, "", 0)
	f.call(t, f.h.Get, http.MethodGet, "")
	if _, out, raw := f.call(t, f.h.Get, http.MethodGet, ""); out["error"] == nil {
		t.Fatalf("failure not kept: %s", raw)
	}
	if n := len(f.fake.Calls()); n != 2 {
		t.Fatalf("calls after a cached failure = %d, want 2", n)
	}
	clock = clock.Add(11 * time.Second)
	if _, out, raw := f.call(t, f.h.Get, http.MethodGet, ""); out["account"] == nil || len(f.fake.Calls()) != 3 {
		t.Fatalf("after the failure TTL: %s (calls %d)", raw, len(f.fake.Calls()))
	}

	// A save drops the cache (its answer is a fresh view); so does a test of
	// the key in force.
	f.call(t, f.h.Save, http.MethodPost, `{"mode":"local"}`)
	if n := len(f.fake.Calls()); n != 4 {
		t.Fatalf("calls after save = %d, want 4", n)
	}
	f.call(t, f.h.Test, http.MethodPost, `{}`)
	f.call(t, f.h.Get, http.MethodGet, "")
	if n := len(f.fake.Calls()); n != 6 {
		t.Fatalf("calls after test + GET = %d, want 6", n)
	}

	// 60 GETs a minute per admin; 7 used above.
	for i := 0; i < 53; i++ {
		f.call(t, f.h.Get, http.MethodGet, "")
	}
	if code, out, raw := f.call(t, f.h.Get, http.MethodGet, ""); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("61st GET: %d %s", code, raw)
	}
}

func TestProviderURLRefusesPlainHTTPOutsideTests(t *testing.T) {
	allowLoopbackHTTP = false
	t.Cleanup(func() { allowLoopbackHTTP = true })
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	body := fmt.Sprintf(`{"mode":"local","url":%s,"key":"k"}`, jsonStr(f.fake.URL()))
	for _, h := range []http.HandlerFunc{f.h.Save, f.h.Test} {
		if code, out, raw := f.call(t, h, http.MethodPost, body); code != 400 || out["code"] != "invalid_url" {
			t.Fatalf("loopback http outside tests: %d %s", code, raw)
		}
	}
	if len(f.fake.Calls()) != 0 || len(f.st.settings) != 0 {
		t.Fatalf("calls %v, settings %v", f.fake.Calls(), f.st.settings)
	}
}

func TestProviderSaveAnnouncesFeaturesChanged(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	sub := rdb.Subscribe(context.Background(), services.SystemEventsChannel)
	t.Cleanup(func() { sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := newProviderFixture(t, config.Config{}, map[string]string{})
	f.h.d.Events = services.NewSystemEventsPublisher(rdb)
	if code, _, raw := f.call(t, f.h.Save, http.MethodPost, `{"mode":"local"}`); code != 200 {
		t.Fatalf("save: %d %s", code, raw)
	}
	select {
	case m := <-sub.Channel():
		if !strings.Contains(m.Payload, `"features.changed"`) || !strings.Contains(m.Payload, "gatewayProvider") {
			t.Fatalf("event = %s", m.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no features.changed event after a save")
	}
}

func TestSystemFeaturesReportGatewayProvider(t *testing.T) {
	st := &modConfigsFlagStore{settings: map[string]string{}}
	state := withCoreStorage(Deps{Store: st, FeatureFlags: services.NewFeatureFlags(st)})
	for _, tc := range []struct {
		name string
		mode func() (bool, bool)
		want string
	}{
		{"no mode wired", nil, "local"},
		{"local", func() (bool, bool) { return false, true }, "local"},
		{"remote", func() (bool, bool) { return true, true }, "remote"},
		{"unknown", func() (bool, bool) { return true, false }, "local"},
	} {
		rec := httptest.NewRecorder()
		deps := health.Deps{Store: st, FeatureFlags: state.FeatureFlags, CoreStorage: state.CoreStorage, GatewayRemote: tc.mode}
		health.NewSystemFeaturesHandler(deps).Get(rec, httptest.NewRequest(http.MethodGet, "/api/system/features", nil))
		var out struct {
			Features map[string]any `json:"features"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Features["gatewayProvider"] != tc.want {
			t.Errorf("%s: gatewayProvider = %v, want %s", tc.name, out.Features["gatewayProvider"], tc.want)
		}
	}
}
