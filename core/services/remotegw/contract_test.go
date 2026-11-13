package remotegw

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func fakeGet(t *testing.T, f *Fake, path string) map[string]interface{} {
	t.Helper()
	req, _ := http.NewRequest("GET", f.URL()+apiPrefix+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.Key())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

func requireKeys(t *testing.T, what string, obj map[string]interface{}, keys []string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			t.Errorf("%s lacks %q: %v", what, k, obj)
		}
	}
}

func first(t *testing.T, obj map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	items, _ := obj[key].([]interface{})
	if len(items) == 0 {
		t.Fatalf("%q is empty: %v", key, obj)
	}
	return items[0].(map[string]interface{})
}

// The same ContractKeys are asserted against the real handlers in
// handlers/gateway/external_contract_test.go.
func TestFakeMatchesProviderShapes(t *testing.T) {
	f := NewFake(t)
	cl := New(f.URL(), f.Key)
	ctx := context.Background()
	m, _ := cl.MintKit(ctx, "k", "eu")
	cl.PutRoute(ctx, m.LinkID, "play.example.com", "mc-12-abc123def456", 25565)
	K := ContractKeys

	acct := fakeGet(t, f, "/account")
	requireKeys(t, "account", acct["account"].(map[string]interface{}), K["account"])
	requireKeys(t, "account.limits", acct["account"].(map[string]interface{})["limits"].(map[string]interface{}), K["account.limits"])
	requireKeys(t, "check-domain", fakeGet(t, f, "/check-domain?domain=a.example.com"), K["check-domain"])
	kits := fakeGet(t, f, "/link-kits")
	requireKeys(t, "link-kits", kits, K["link-kits"])
	requireKeys(t, "link-kits.item", first(t, kits, "items"), K["link-kits.item"])
	routes := fakeGet(t, f, "/routes")
	requireKeys(t, "routes", routes, K["routes"])
	requireKeys(t, "routes.item", first(t, routes, "items"), K["routes.item"])
	regions := fakeGet(t, f, "/regions")
	requireKeys(t, "regions", regions, K["regions"])
	requireKeys(t, "regions.item", first(t, regions, "items"), K["regions.item"])
	usage := fakeGet(t, f, "/usage")
	requireKeys(t, "usage", usage, K["usage"])
	requireKeys(t, "usage.usage", usage["usage"].(map[string]interface{}), K["usage.usage"])

	// Mint has no GET: drive it raw.
	req, _ := http.NewRequest("POST", f.URL()+apiPrefix+"/link-kits", nil)
	req.Header.Set("Authorization", "Bearer "+f.Key())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var mint map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&mint)
	requireKeys(t, "link-kits.mint", mint, K["link-kits.mint"])

	f.SetLinkLimit(5)
	req, _ = http.NewRequest("POST", f.URL()+apiPrefix+"/link-kits/"+m.LinkID+"/retire", nil)
	req.Header.Set("Authorization", "Bearer "+f.Key())
	rresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rresp.Body.Close()
	var retire map[string]interface{}
	json.NewDecoder(rresp.Body).Decode(&retire)
	requireKeys(t, "link-kits.retire", retire, K["link-kits.retire"])
}
