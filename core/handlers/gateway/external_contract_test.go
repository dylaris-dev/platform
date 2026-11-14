package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
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
// provider.
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

	retire := get(func(rec *httptest.ResponseRecorder) {
		f.h.RetireLinkKit(rec, retireReq(extTenantB, "link-b"))
	})
	contractRequire(t, "link-kits.retire", retire, K["link-kits.retire"])
}

// Kit mint and usage through the REAL handlers: WarpHandler.MintLinkKit behind
// the external adapter, and billing.UsageHandler.GetMyUsage as the usage
// handler, for a tenant holding a managed plan.
func TestProviderMatchesClientContractMintAndUsage(t *testing.T) {
	K := remotegw.ContractKeys
	f := newExtFixture(t, withManagedOwner(1, 5000))

	mint := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.h.MintLinkKit(rec, extReq("POST", "/", extTenantC, false, nil, map[string]string{"name": "c1"}))
		return rec
	}
	rec := mint()
	if rec.Code != 200 {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	contractRequire(t, "link-kits.mint", decodeExt(t, rec), K["link-kits.mint"])

	if rec := mint(); rec.Code != 403 || decodeExt(t, rec)["code"] != "link_limit" {
		t.Fatalf("mint past maxLinks: %d %s, want 403 link_limit", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	f.h.GetUsage(rec, extReq("GET", "/", extTenantC, false, nil, nil))
	if rec.Code != 200 {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body.String())
	}
	usage := decodeExt(t, rec)
	contractRequire(t, "usage", usage, K["usage"])
	inner, ok := usage["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage is not an object: %v", usage)
	}
	contractRequire(t, "usage.usage", inner, K["usage.usage"])
}

// The own-domain endpoints through the REAL handlers, keys and the outcomes the
// customer client maps (remotegw.VerifyCustomDomainTXT and DeleteCustomDomain):
// the fake in remotegwtest answers each of them the same way.
func TestProviderMatchesClientContractCustomDomains(t *testing.T) {
	K := remotegw.ContractKeys
	f := newExtFixture(t)
	call := func(fn func(*ExternalGatewayHandler, http.ResponseWriter, *http.Request), method, owner string, vars map[string]string, body interface{}) (int, map[string]interface{}) {
		rec := httptest.NewRecorder()
		fn(f.h, rec, extReq(method, "/", owner, false, vars, body))
		return rec.Code, decodeExt(t, rec)
	}

	code, list := call((*ExternalGatewayHandler).ListCustomDomains, "GET", extTenantB, nil, nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, list)
	}
	contractRequire(t, "custom-domains", list, K["custom-domains"])
	contractRequire(t, "custom-domains.item", contractFirst(t, list), K["custom-domains.item"])

	code, add := call((*ExternalGatewayHandler).AddCustomDomain, "POST", extTenantC, nil, map[string]string{"domain": "c-own.net"})
	if code != 200 {
		t.Fatalf("add: %d %v", code, add)
	}
	contractRequire(t, "custom-domains.add", add, K["custom-domains.add"])
	if d, _ := add["domain"].(map[string]interface{}); d["txtValue"] == nil || d["txtName"] != "_dylaris-verify.c-own.net" {
		t.Fatalf("add carries no TXT record: %v", add)
	}

	code, txt := call((*ExternalGatewayHandler).IssueCustomDomainTXTToken, "POST", extTenantA, map[string]string{"domain": "a-own.net"}, nil)
	if code != 200 {
		t.Fatalf("txt-token: %d %v", code, txt)
	}
	contractRequire(t, "custom-domains.txt-token", txt, K["custom-domains.txt-token"])

	verify := func(owner, domain string) (int, string) {
		code, body := call((*ExternalGatewayHandler).VerifyCustomDomainTXT, "POST", owner, map[string]string{"domain": domain}, nil)
		c, _ := body["code"].(string)
		return code, c
	}
	for _, c := range []struct {
		name, owner, domain string
		status              int
		code                string
	}{
		{"record not found yet: plain 409", extTenantA, "a-own.net", 409, "conflict"},
		{"checked again at once: cooldown", extTenantA, "a-own.net", 429, "verify_cooldown"},
		{"already verified", extTenantB, "b-own.net", 400, "bad_request"},
		{"no such claim", extTenantA, "nobody.net", 404, "not_found"},
	} {
		if status, code := verify(c.owner, c.domain); status != c.status || code != c.code {
			t.Errorf("verify %s: %d %q, want %d %q", c.name, status, code, c.status, c.code)
		}
	}

	seedLinkRoute(t, f.rdb, "b-own.net", services.GatewayRoute{CoreOwned: true, OwnerID: extTenantB,
		TunnelID: "derived-token-link-b", TargetIP: "192.168.1.20", TargetPort: 25565})
	if code, body := call((*ExternalGatewayHandler).DeleteCustomDomain, "DELETE", extTenantB, map[string]string{"domain": "b-own.net"}, nil); code != 409 || body["code"] != "domain_in_use" {
		t.Errorf("delete while routed: %d %v, want 409 domain_in_use", code, body)
	}
	if code, body := call((*ExternalGatewayHandler).DeleteCustomDomain, "DELETE", extTenantB, map[string]string{"domain": "nobody.net"}, nil); code != 404 || body["code"] != "not_found" {
		t.Errorf("delete of no claim: %d %v, want 404 not_found", code, body)
	}

	for i := 0; i < maxClaimsPerUser; i++ {
		f.st.claims = append(f.st.claims, store.CustomDomainClaim{ID: 1000 + i, UserID: extTenantC, Domain: fmt.Sprintf("c%d.net", i), State: store.ClaimPending})
	}
	if code, body := call((*ExternalGatewayHandler).AddCustomDomain, "POST", extTenantC, nil, map[string]string{"domain": "one-more.net"}); code != 409 || body["code"] != "too_many_domains" {
		t.Errorf("add past the bound: %d %v, want 409 too_many_domains", code, body)
	}
	for _, c := range []struct{ domain, code string }{
		{"x.example.com", "bad_request"},
		{"bad_name.net", "bad_request"},
		{"a.b.c.d.e.net", "bad_request"},
	} {
		if code, body := call((*ExternalGatewayHandler).AddCustomDomain, "POST", extTenantA, nil, map[string]string{"domain": c.domain}); code != 400 || body["code"] != c.code {
			t.Errorf("add %s: %d %v, want 400 %s", c.domain, code, body, c.code)
		}
	}
	if code, body := call((*ExternalGatewayHandler).AddCustomDomain, "POST", extTenantA, nil, map[string]string{"domain": "Trail.own.net."}); code != 200 {
		t.Errorf("add with a trailing dot: %d %v", code, body)
	} else if d, _ := body["domain"].(map[string]interface{}); d["domain"] != "trail.own.net" {
		t.Errorf("trailing dot not trimmed: %v", body)
	}
	f.st.settings["gateway_custom_domains_enabled"] = "false"
	if code, body := call((*ExternalGatewayHandler).AddCustomDomain, "POST", extTenantA, nil, map[string]string{"domain": "a2-own.net"}); code != 409 || body["code"] != "custom_domains_off" {
		t.Errorf("add with own domains off: %d %v, want 409 custom_domains_off", code, body)
	}
}
