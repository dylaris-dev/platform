package gateway

import (
	"context"
	"net/http/httptest"
	"testing"

	"dylaris-core/services/remotegw"
)

func contractRequire(t *testing.T, what string, obj map[string]interface{}, keys []string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			t.Errorf("provider %s lacks %q the client depends on: %v", what, k, obj)
		}
	}
}

func contractFirst(t *testing.T, obj map[string]interface{}) map[string]interface{} {
	t.Helper()
	items, _ := obj["items"].([]interface{})
	if len(items) == 0 {
		t.Fatalf("no items: %v", obj)
	}
	return items[0].(map[string]interface{})
}

// The same remotegw.ContractKeys are asserted against the fake in
// services/remotegw/contract_test.go, so the fake cannot drift from the
// provider. Not covered here: mint (POST /link-kits needs a full warp and
// entitlement fixture) and usage (the fixture stubs the billing handler).
func TestProviderMatchesClientContract(t *testing.T) {
	K := remotegw.ContractKeys
	f := newExtFixture(t)
	ctx := context.Background()
	f.rdb.Set(ctx, "online_link:derived-token-link-a", `{"caps":["client-addr"]}`, 0)
	f.rdb.Set(ctx, "edge:registry:e1", `{"edge_id":"e1","ip":"203.0.113.9","region":"eu","wildcard":"*.eu.dylaris.com","status":"online"}`, 0)

	get := func(fn func(rec *httptest.ResponseRecorder)) map[string]interface{} {
		rec := httptest.NewRecorder()
		fn(rec)
		if rec.Code != 200 {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return decodeExt(t, rec)
	}

	acct := getAccount(t, f, extTenantA, []string{"gateway.read"})
	contractRequire(t, "account", acct, K["account"])
	contractRequire(t, "account.limits", acct["limits"].(map[string]interface{}), K["account.limits"])

	contractRequire(t, "check-domain", checkDomain(t, f, extTenantA, "free.example.com"), K["check-domain"])

	kits := get(func(rec *httptest.ResponseRecorder) {
		f.h.ListLinkKits(rec, extReq("GET", "/", extTenantA, false, nil, nil))
	})
	contractRequire(t, "link-kits", kits, K["link-kits"])
	contractRequire(t, "link-kits.item", contractFirst(t, kits), K["link-kits.item"])

	routes := get(func(rec *httptest.ResponseRecorder) {
		f.h.ListRoutes(rec, extReq("GET", "/", extTenantA, false, nil, nil))
	})
	contractRequire(t, "routes", routes, K["routes"])
	contractRequire(t, "routes.item", contractFirst(t, routes), K["routes.item"])

	regions := get(func(rec *httptest.ResponseRecorder) {
		f.h.ListRegions(rec, extReq("GET", "/", extTenantA, false, nil, nil))
	})
	contractRequire(t, "regions", regions, K["regions"])
	contractRequire(t, "regions.item", contractFirst(t, regions), K["regions.item"])
}
