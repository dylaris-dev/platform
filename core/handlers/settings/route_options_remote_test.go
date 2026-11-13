package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
	"dylaris-core/services/remotegw"
)

// On the managed gateway the domain picker offers the provider's hoster
// domains, never the local settings, and custom domains are off.
func TestRemoteRouteOptionsComeFromProviderRegions(t *testing.T) {
	fake := remotegw.NewFake(t)
	// No store: a read of the local settings would panic.
	rg := services.NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	h := &SettingsHandler{d: Deps{GatewayRemote: func() (bool, bool) { return true, true }, RemoteGateway: rg}}

	get := func() (int, map[string]interface{}) {
		rec := httptest.NewRecorder()
		h.GetGatewayRouteOptions(rec, httptest.NewRequest(http.MethodGet, "/api/gateway/route-options", nil))
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := get()
	hosters, _ := out["hosterDomains"].([]interface{})
	if code != 200 || out["success"] != true || len(hosters) != 1 || out["customDomainsEnabled"] != false || out["cnameTarget"] != "" {
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
