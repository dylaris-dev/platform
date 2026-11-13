package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// acctStore gives each tenant its own billing row, so product and suspension
// can differ per test. A tenant without a row has bought nothing.
type acctStore struct {
	*extStore
	billing map[string]*store.UserBilling
}

func (s *acctStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return s.billing[userID], nil
}

// newAcctFixture is the external fixture on a hosted install (store enabled),
// where an account that bought nothing is entitled to nothing.
func newAcctFixture(t *testing.T, billing map[string]*store.UserBilling) *extFixture {
	t.Helper()
	f := newExtFixture(t)
	f.h.gw.d.Store = &acctStore{extStore: f.st, billing: billing}
	f.h.gw.d.StoreEnabled = true
	return f
}

func getAccount(t *testing.T, f *extFixture, owner string, perms []string) map[string]interface{} {
	t.Helper()
	r := extReq("GET", "/api/external/gateway/account", owner, false, nil, nil)
	r = r.WithContext(reqctx.WithAPIKey(r.Context(), &models.APIKey{ID: 7, UserID: owner, Scope: models.APIKeyScope{Permissions: perms}}, false))
	rec := httptest.NewRecorder()
	f.h.GetAccount(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeExt(t, rec)
	acct, ok := out["account"].(map[string]interface{})
	if out["success"] != true || !ok {
		t.Fatalf("want success + account: %s", rec.Body.String())
	}
	return acct
}

func TestExternalAccountReportsProductAndScopes(t *testing.T) {
	five := int64(5)
	f := newAcctFixture(t, map[string]*store.UserBilling{
		extTenantA: {UserID: extTenantA, Status: "active", MaxLinks: &five},
		extTenantB: {UserID: extTenantB, Status: "active", MaxLinks: &five},
	})
	// Only the store writes a user-scope edge row, and only for a managed plan.
	f.st.limits = map[string]*models.TrafficLimit{"user:" + extTenantA: {}}

	scopes := []string{"gateway.read", "usage.read"}
	for _, c := range []struct {
		owner, product string
		managed        bool
	}{
		{extTenantA, "managed", true},
		{extTenantB, "route_only", false},
		{extTenantC, "none", false},
	} {
		t.Run(c.product, func(t *testing.T) {
			acct := getAccount(t, f, c.owner, scopes)
			if acct["product"] != c.product || acct["managed"] != c.managed || acct["userId"] != c.owner {
				t.Fatalf("account = %v, want product %s managed %v", acct, c.product, c.managed)
			}
			if acct["suspended"] != false {
				t.Fatalf("suspended = %v, want false", acct["suspended"])
			}
			got, _ := acct["scopes"].([]interface{})
			if len(got) != len(scopes) || got[0] != scopes[0] || got[1] != scopes[1] {
				t.Fatalf("scopes = %v, want %v", acct["scopes"], scopes)
			}
			lim, _ := acct["limits"].(map[string]interface{})
			for _, k := range []string{"maxLinks", "maxRoutes", "maxCustomDomains"} {
				if _, ok := lim[k]; !ok {
					t.Fatalf("limits lacks %q (null is a value, absent is a bug): %v", k, lim)
				}
			}
		})
	}
	if lim := getAccount(t, f, extTenantA, nil)["limits"].(map[string]interface{}); lim["maxLinks"] != float64(5) {
		t.Errorf("maxLinks = %v, want 5", lim["maxLinks"])
	}
	if got := getAccount(t, f, extTenantA, nil)["scopes"]; !reflect.DeepEqual(got, []interface{}{}) {
		t.Errorf("a key without permissions must give [] not %#v", got)
	}
}

func TestExternalAccountSuspended(t *testing.T) {
	five := int64(5)
	f := newAcctFixture(t, map[string]*store.UserBilling{
		extTenantA: {UserID: extTenantA, Status: "suspended", MaxLinks: &five},
	})
	f.st.limits = map[string]*models.TrafficLimit{"user:" + extTenantA: {}}
	acct := getAccount(t, f, extTenantA, []string{"gateway.read"})
	if acct["suspended"] != true || acct["entitlementSource"] != "suspended" {
		t.Fatalf("account = %v, want suspended", acct)
	}
	// Still a managed customer, so the panel can say who is suspended.
	if acct["product"] != "managed" {
		t.Errorf("product = %v, want managed", acct["product"])
	}
}

func checkDomain(t *testing.T, f *extFixture, owner, domain string) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.CheckDomain(rec, extReq("GET", "/api/external/gateway/check-domain?domain="+domain, owner, false, nil, nil))
	if rec.Code != 200 {
		t.Fatalf("%s: status = %d: %s", domain, rec.Code, rec.Body.String())
	}
	return decodeExt(t, rec)
}

func TestExternalCheckDomain(t *testing.T) {
	f := newExtFixture(t)
	for _, c := range []struct {
		name, domain string
		available    bool
		reason       string
	}{
		{"free", "free.example.com", true, ""},
		{"taken by another tenant", "b.example.com", false, "taken"},
		{"own domain", "a.example.com", true, ""},
		{"blocked prefix", "admin.example.com", false, "blocked"},
		{"malformed", "bad_name!.example.com", false, "invalid"},
		{"hoster apex", "example.com", false, "invalid"},
		{"empty", "", false, "invalid"},
		{"own domain shape", "play.acme.net", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := checkDomain(t, f, extTenantA, c.domain)
			if out["success"] != true || out["available"] != c.available || out["reason"] != c.reason {
				t.Fatalf("answer = %v, want available %v reason %q", out, c.available, c.reason)
			}
		})
	}

	t.Run("not hosted", func(t *testing.T) {
		f := newExtFixture(t)
		f.st.settings["gateway_custom_domains_enabled"] = "false"
		out := checkDomain(t, f, extTenantA, "play.acme.net")
		if out["available"] != false || out["reason"] != "not_hosted" {
			t.Fatalf("answer = %v, want not_hosted", out)
		}
	})
}

func TestExternalAccountNeverLeaksOtherOwner(t *testing.T) {
	f := newExtFixture(t)
	rec := httptest.NewRecorder()
	f.h.CheckDomain(rec, extReq("GET", "/api/external/gateway/check-domain?domain=b.example.com", extTenantA, false, nil, nil))
	out := decodeExt(t, rec)
	if out["available"] != false || out["reason"] != "taken" {
		t.Fatalf("answer = %v, want taken", out)
	}
	if len(out) != 4 {
		t.Errorf("answer has %d fields, want exactly success, domain, available, reason: %v", len(out), out)
	}
	for _, leak := range []string{extTenantB, "192.168.1.20", "derived-token-link-b", "tenant-b-kit"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("answer leaks %q: %s", leak, rec.Body.String())
		}
	}
}

func TestExternalKitListCarriesRegionAndCaps(t *testing.T) {
	f := newExtFixture(t)
	f.st.kits = []store.WarpAPIKey{
		{ID: 1, Name: "on", NodeID: "link-on", OwnerID: extTenantA, Region: "eu"},
		{ID: 2, Name: "off", NodeID: "link-off", OwnerID: extTenantA},
		{ID: 3, Name: "old", NodeID: "link-old", OwnerID: extTenantA},
	}
	ctx := context.Background()
	f.rdb.Set(ctx, "online_link:derived-token-link-on", `{"caps":["client-addr","voice"]}`, 0)
	f.rdb.Set(ctx, "online_link:derived-token-link-old", "1", 0) // an old link writes "1", no caps

	rec := httptest.NewRecorder()
	f.h.ListLinkKits(rec, extReq("GET", "/api/external/gateway/link-kits", extTenantA, false, nil, nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	items, _ := decodeExt(t, rec)["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3: %s", len(items), rec.Body.String())
	}
	want := map[string]struct {
		region string
		caps   []interface{}
	}{
		"link-on":  {"eu", []interface{}{"client-addr", "voice"}},
		"link-off": {"", []interface{}{}},
		"link-old": {"", []interface{}{}},
	}
	for _, it := range items {
		m := it.(map[string]interface{})
		w := want[m["link_id"].(string)]
		if m["region"] != w.region || !reflect.DeepEqual(m["caps"], w.caps) {
			t.Errorf("%v: region %v caps %#v, want %q %#v", m["link_id"], m["region"], m["caps"], w.region, w.caps)
		}
	}
}

// ownedFailStore fails the owned-servers lookup CheckDomain needs to tell a
// held route from a taken one.
type ownedFailStore struct{ *extStore }

func (s *ownedFailStore) ListServersByOwner(string) ([]models.Server, error) {
	return nil, errors.New("db down")
}

func TestExternalCheckDomainFailsOnLookupErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		break_ func(f *extFixture)
	}{
		{"redis read fails", func(f *extFixture) { f.rdb.Close() }},
		{"owned servers lookup fails", func(f *extFixture) { f.h.gw.d.Store = &ownedFailStore{f.st} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newExtFixture(t)
			// Redis only: a stored row would answer before the lookups run.
			f.st.routes = f.st.routes[:1]
			c.break_(f)
			// b.example.com has a route, so the owner check runs; a failure must
			// not be answered as taken (or free).
			rec := httptest.NewRecorder()
			f.h.CheckDomain(rec, extReq("GET", "/api/external/gateway/check-domain?domain=b.example.com", extTenantA, false, nil, nil))
			if rec.Code != 500 {
				t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
			}
			if out := decodeExt(t, rec); out["code"] != "internal" {
				t.Fatalf("answer = %v, want code internal", out)
			}
		})
	}
}

// The stored row answers first, as on route create: another owner's row is
// taken even with Redis emptied, and the caller's own suspended route is not
// available (blocked) rather than "yours, free".
func TestExternalCheckDomainAsksTheStoredRowFirst(t *testing.T) {
	f := newExtFixture(t)
	f.rdb.Del(context.Background(), "route:b.example.com", "route:a.example.com")
	if out := checkDomain(t, f, extTenantA, "b.example.com"); out["available"] != false || out["reason"] != "taken" {
		t.Fatalf("another owner's row, Redis empty: %v, want taken", out)
	}
	at := time.Now()
	f.st.routes[0].SuspendedAt = &at
	if out := checkDomain(t, f, extTenantA, "a.example.com"); out["available"] != false || out["reason"] != "blocked" {
		t.Fatalf("own suspended route: %v, want blocked", out)
	}
	// Another owner asking about the suspended route learns only "taken".
	if out := checkDomain(t, f, extTenantB, "a.example.com"); out["available"] != false || out["reason"] != "taken" {
		t.Fatalf("someone else's suspended route: %v, want taken", out)
	}
}

// roundTrips counts the client's trips to Redis: a pipeline is one.
type roundTrips struct{ n int }

func (h *roundTrips) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *roundTrips) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { h.n++; return next(ctx, cmd) }
}
func (h *roundTrips) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error { h.n++; return next(ctx, cmds) }
}

// The kit list asks Redis about every kit in one trip, online and caps alike;
// it used to be one trip for the set plus one per online kit.
func TestKitListAsksRedisOnce(t *testing.T) {
	f := newExtFixture(t)
	f.st.kits = []store.WarpAPIKey{
		{ID: 1, Name: "on", NodeID: "link-on", OwnerID: extTenantA},
		{ID: 2, Name: "on2", NodeID: "link-on2", OwnerID: extTenantA},
		{ID: 3, Name: "off", NodeID: "link-off", OwnerID: extTenantA},
	}
	ctx := context.Background()
	f.rdb.Set(ctx, "online_link:derived-token-link-on", `{"caps":["client-addr"]}`, 0)
	f.rdb.Set(ctx, "online_link:derived-token-link-on2", `{"caps":["voice"]}`, 0)
	hook := &roundTrips{}
	f.rdb.AddHook(hook)
	rec := httptest.NewRecorder()
	f.h.ListLinkKits(rec, extReq("GET", "/api/external/gateway/link-kits", extTenantA, false, nil, nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if hook.n != 1 {
		t.Fatalf("redis round trips = %d, want 1", hook.n)
	}
	items, _ := decodeExt(t, rec)["items"].([]interface{})
	got := map[string]string{}
	for _, it := range items {
		m := it.(map[string]interface{})
		got[m["link_id"].(string)] = strings.TrimSpace(strings.Join(func() []string {
			var s []string
			for _, c := range m["caps"].([]interface{}) {
				s = append(s, c.(string))
			}
			return s
		}(), ","))
		if on, _ := m["online"].(bool); on != (m["link_id"] != "link-off") {
			t.Errorf("%v online = %v", m["link_id"], m["online"])
		}
	}
	if got["link-on"] != "client-addr" || got["link-on2"] != "voice" || got["link-off"] != "" {
		t.Fatalf("caps = %v", got)
	}
}
