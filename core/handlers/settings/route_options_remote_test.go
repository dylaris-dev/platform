package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
)

// On the managed gateway the domain picker offers the provider's hoster
// domains, never the local settings.
func TestRemoteRouteOptionsComeFromProviderRegions(t *testing.T) {
	fake := remotegwtest.NewFake(t)
	// The only local setting read is the own-domain switch.
	st := &modConfigsFlagStore{settings: map[string]string{"gateway_hoster_domains": `[{"domain":"local.tld","validation":"dns"}]`}}
	rg := services.NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	h := &SettingsHandler{d: Deps{Store: st, GatewayRemote: func() (bool, bool) { return true, true }, RemoteGateway: rg}}

	get := func() (int, map[string]interface{}) {
		rec := httptest.NewRecorder()
		h.GetGatewayRouteOptions(rec, httptest.NewRequest(http.MethodGet, "/api/gateway/route-options", nil))
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := get()
	hosters, _ := out["hosterDomains"].([]interface{})
	if code != 200 || out["success"] != true || len(hosters) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	if hd, _ := hosters[0].(map[string]interface{}); hd["domain"] != "example.com" || hd["validation"] != "dns" {
		t.Fatalf("hoster = %v", hosters[0])
	}
	get()
	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("provider calls = %v, want 1 (cached)", fake.Calls())
	}

	h.d.RemoteGateway = services.NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	fake.FailNext(1, 429, "rate_limited", 0)
	if code, out := get(); code != http.StatusServiceUnavailable || out["code"] != "rate_limited" {
		t.Fatalf("provider refusal: %d %v", code, out)
	}
}

// Own domains are offered on the managed gateway only when the provider allows
// them for the account AND the local setting does, for admins too as in local
// mode; the CNAME target is the provider's.
func TestRemoteRouteOptionsCustomFlag(t *testing.T) {
	for _, c := range []struct {
		name            string
		provider, admin bool
		setting         string
		want            bool
	}{
		{"both on", true, false, "true", true},
		{"provider off", false, false, "true", false},
		{"local off", true, false, "", false},
		{"local off, admin", true, true, "false", false},
		{"both on, admin", true, true, "true", true},
		{"provider off, admin", false, true, "true", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := remotegwtest.NewFake(t)
			fake.SetCustomDomainsEnabled(c.provider)
			st := &modConfigsFlagStore{settings: map[string]string{"gateway_custom_domains_enabled": c.setting}}
			rg := services.NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
			h := &SettingsHandler{d: Deps{Store: st, GatewayRemote: func() (bool, bool) { return true, true }, RemoteGateway: rg}}
			r := httptest.NewRequest(http.MethodGet, "/api/gateway/route-options", nil)
			//lint:ignore SA1029 the handlers read plain string keys.
			r = r.WithContext(context.WithValue(r.Context(), "isAdmin", c.admin))
			rec := httptest.NewRecorder()
			h.GetGatewayRouteOptions(rec, r)
			var out map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if rec.Code != 200 || out["customDomainsEnabled"] != c.want || out["cnameTarget"] != "route.example.com" {
				t.Fatalf("%d %v, want customDomainsEnabled %v", rec.Code, out, c.want)
			}
		})
	}
}

// With the local switch off both modes tell an admin the same: no own domains.
func TestRouteOptionsSwitchOffAgreesInBothModes(t *testing.T) {
	st := &modConfigsFlagStore{settings: map[string]string{"gateway_custom_domains_enabled": "false"}}
	fake := remotegwtest.NewFake(t)
	rg := services.NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	for name, d := range map[string]Deps{
		"local":  {Store: st},
		"remote": {Store: st, GatewayRemote: func() (bool, bool) { return true, true }, RemoteGateway: rg},
	} {
		h := &SettingsHandler{d: d}
		r := httptest.NewRequest(http.MethodGet, "/api/gateway/route-options", nil)
		//lint:ignore SA1029 the handlers read plain string keys.
		r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
		rec := httptest.NewRecorder()
		h.GetGatewayRouteOptions(rec, r)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 200 || out["customDomainsEnabled"] != false {
			t.Fatalf("%s: %d %v", name, rec.Code, out)
		}
	}
}
