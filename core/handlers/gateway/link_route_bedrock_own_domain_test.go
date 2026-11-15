package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/store"
)

const usEdgeAddr = "203.0.113.20"

// newBedrockOwnDomainEnv is newOwnDomainEnv with Bedrock offered to everyone
// (so the panel path may add it) and the eu entry newBedrockSwitch seeds.
func newBedrockOwnDomainEnv(t *testing.T) (*managedEnv, *odResolver) {
	t.Helper()
	m, res := newOwnDomainEnv(t)
	m.ms.audience = "all"
	return m, res
}

func (m *managedEnv) panelBedrock(domain, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	rec := linkCall(m.e, m.e.h.SetLinkRouteBedrock, http.MethodPut, domain, body, "owner-1", false)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (m *managedEnv) extBedrockOn(domain string, body map[string]interface{}) (int, map[string]interface{}) {
	rec := httptest.NewRecorder()
	m.x.SetRouteBedrock(rec, extReq("PUT", "/", "owner-1", false, map[string]string{"domain": domain}, body))
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

const bedrockOn = `{"java":true,"bedrock":true}`

func bedrockRefusalText(status services.PointingStatus, found ...string) string {
	return services.PointingRefusal(services.Pointing{Status: status, Found: found}, odDomain, nil, services.PurposeBedrock)
}

// Every outcome of the add on an own domain: ok with an entry adds Bedrock and
// stores the check; every known "no" refuses with its sentence and is stored
// as a miss; "could not check" stores and changes nothing.
func TestLinkRouteBedrockOwnDomain(t *testing.T) {
	cases := []struct {
		name    string
		dns     func(m *managedEnv, r *odResolver)
		status  int
		code    string
		message string
		stored  services.PointingStatus // "" = no row
	}{
		{"ok with entry", func(_ *managedEnv, r *odResolver) { r.a[odDomain] = addrs(edgeAddr) }, 200, "", "", services.PointingOK},
		{"ok by cname with entry", func(_ *managedEnv, r *odResolver) { r.cname[odDomain] = "route.eu.dylaris.com" }, 200, "", "", services.PointingOK},
		{"ok without entry", func(m *managedEnv, r *odResolver) {
			r.a[odDomain] = addrs(edgeAddr)
			m.e.mr.Del("bedrock:registry:b1")
		}, 409, "bedrock_unavailable", bedrockNoEntry, services.PointingOK},
		{"srv only", func(_ *managedEnv, r *odResolver) {
			r.srv["_minecraft._tcp."+odDomain] = []*net.SRV{{Target: "route.eu.dylaris.com.", Port: 25565}}
		}, 409, "bedrock_unavailable", odDomain + " reaches the gateway only through an SRV record, and Bedrock ignores SRV records. " +
			"Add a CNAME from " + odDomain + " to the address shown under Your own domains, or give Bedrock to an address on one of the gateway's domains.",
			services.PointingSRVOnly},
		{"elsewhere", func(_ *managedEnv, r *odResolver) { r.a[odDomain] = addrs(foreignAddr) }, 409, "bedrock_unavailable",
			odDomain + " points to " + foreignAddr + ", not to the gateway. Add a CNAME from " + odDomain + " to the address shown under Your own domains. " +
				"A proxy or DDoS filter in front of the domain cannot carry Bedrock.", services.PointingElsewhere},
		{"partial", func(_ *managedEnv, r *odResolver) { r.a[odDomain] = addrs(edgeAddr, foreignAddr) }, 409, "bedrock_unavailable",
			bedrockRefusalText(services.PointingPartial, foreignAddr), services.PointingPartial},
		{"multi region", func(m *managedEnv, r *odResolver) {
			m.e.mr.Set("edge:registry:e2", `{"edge_id":"e2","status":"online","region":"us","wildcard":"*.us.dylaris.com","ip":"`+usEdgeAddr+`"}`)
			r.a[odDomain] = addrs(edgeAddr, usEdgeAddr)
		}, 409, "bedrock_unavailable", bedrockRefusalText(services.PointingMultiRegion), services.PointingMultiRegion},
		{"ipv6", func(_ *managedEnv, r *odResolver) {
			r.a[odDomain] = addrs(edgeAddr)
			r.aaaa = map[string][]netip.Addr{odDomain: addrs("2001:db8::1")}
		}, 409, "bedrock_unavailable", bedrockRefusalText(services.PointingIPv6), services.PointingIPv6},
		{"ipv6 at the end of an alias", func(_ *managedEnv, r *odResolver) {
			r.cname[odDomain] = "lb.host.net"
			r.a["lb.host.net"] = addrs(edgeAddr)
			r.aaaa = map[string][]netip.Addr{"lb.host.net": addrs("2001:db8::1")}
		}, 409, "bedrock_unavailable", bedrockRefusalText(services.PointingIPv6), services.PointingIPv6},
		{"private", func(_ *managedEnv, r *odResolver) { r.a[odDomain] = addrs("10.0.0.5") }, 409, "bedrock_unavailable",
			odDomain + " resolves to an address that is not on the public internet.", services.PointingPrivate},
		{"lookup failed", func(_ *managedEnv, r *odResolver) {
			r.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		}, 503, "dns_lookup_failed",
			"The lookup of " + odDomain + " failed right now. Nothing was changed. Try again in a moment.", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, res := newBedrockOwnDomainEnv(t)
			c.dns(m, res)
			rec, out := m.panelBedrock(odDomain, bedrockOn)
			if rec.Code != c.status {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), c.status)
			}
			row, stored := m.ms.dns[odDomain]
			if c.stored == "" {
				if stored {
					t.Fatalf("an unknown answer was stored: %+v", row)
				}
			} else if !stored || row.Status != string(c.stored) || (c.stored == services.PointingOK) != (row.Misses == 0 && row.FailingSince == nil) {
				t.Fatalf("stored %+v (%v), want %s", row, stored, c.stored)
			}
			if c.status != 200 {
				if out["code"] != c.code || out["message"] != c.message {
					t.Fatalf("answer %v, want %s %q", out, c.code, c.message)
				}
				if len(m.ms.links) != 0 || m.e.mr.Exists(services.RouteEditionsKey) {
					t.Fatal("a refusal added Bedrock")
				}
				return
			}
			b := out["bedrock"].(map[string]interface{})
			if b["bedrock"] != true || b["dns_status"] != "ok" || b["dns_checked_at"] == nil || b["dns_failing_since"] != nil {
				t.Fatalf("answer %v", b)
			}
			if got := m.e.mr.HGet(services.RouteEditionsKey, odDomain); got != "both" {
				t.Fatalf("edition %q, want both", got)
			}
			pub, _ := m.e.mr.Get(services.FloodgateKeyRedisKey("owner:owner-1:bedrock:" + odDomain))
			if pub != base64.StdEncoding.EncodeToString(m.ms.links[odDomain].Key) {
				t.Fatalf("floodgate key %q not published", pub)
			}
		})
	}

	t.Run("rate limited", func(t *testing.T) {
		m, res := newBedrockOwnDomainEnv(t)
		res.a[odDomain] = addrs(edgeAddr)
		m.e.h.d.PointingLimiter = httpx.NewIPRateLimiter()
		for i := 0; i < pointingChecksPerMin; i++ {
			m.e.h.d.PointingLimiter.Allow("user:owner-1", pointingChecksPerMin)
		}
		rec, out := m.panelBedrock(odDomain, bedrockOn)
		if rec.Code != http.StatusTooManyRequests || out["code"] != "rate_limited" || out["message"] != "Too many DNS checks. Wait a minute." {
			t.Fatalf("%d %v", rec.Code, out)
		}
		if res.count() != 0 || len(m.ms.dns) != 0 || len(m.ms.links) != 0 {
			t.Fatalf("resolver calls %d rows %v links %v", res.count(), m.ms.dns, m.ms.links)
		}
	})

	t.Run("external lookup failed passes the code through", func(t *testing.T) {
		m, res := newBedrockOwnDomainEnv(t)
		res.fail = &net.DNSError{Err: "i/o timeout", IsTimeout: true}
		status, out := m.extBedrockOn(odDomain, map[string]interface{}{"java": true, "bedrock": true})
		if status != 503 || out["code"] != "dns_lookup_failed" || len(m.ms.dns) != 0 {
			t.Fatalf("%d %v rows %v", status, out, m.ms.dns)
		}
	})

	t.Run("a miss after a miss keeps the start of the run", func(t *testing.T) {
		m, res := newBedrockOwnDomainEnv(t)
		res.a[odDomain] = addrs(foreignAddr)
		m.panelBedrock(odDomain, bedrockOn)
		first := *m.ms.dns[odDomain].FailingSince
		m.e.h.d.Pointing = odChecker(m.e.h.d.Redis, res) // no cached verdict
		m.panelBedrock(odDomain, bedrockOn)
		if row := m.ms.dns[odDomain]; row.Misses != 2 || !row.FailingSince.Equal(first) {
			t.Fatalf("row %+v", row)
		}
	})
}

// Review Focus 5: an address that already has Bedrock is re-asserted (the
// 7b-2 sweep, a panel save) without any add check, so a DNS change can never
// turn it into a refusal.
func TestBedrockReassertOfExistingRowNeverResolves(t *testing.T) {
	m, res := newBedrockOwnDomainEnv(t)
	res.a[odDomain] = addrs(foreignAddr)
	key := bytes.Repeat([]byte{7}, 16)
	m.ms.links[odDomain] = store.LinkRouteEdition{Domain: odDomain, OwnerID: "owner-1", Java: true, Bedrock: true, Key: key}
	if rec, _ := m.panelBedrock(odDomain, bedrockOn); rec.Code != 200 {
		t.Fatalf("panel re-assert %d %s", rec.Code, rec.Body.String())
	}
	if status, out := m.extBedrockOn(odDomain, map[string]interface{}{"java": true, "bedrock": true}); status != 200 {
		t.Fatalf("external re-assert %d %v", status, out)
	}
	if res.count() != 0 || len(m.ms.dns) != 0 || !bytes.Equal(m.ms.links[odDomain].Key, key) {
		t.Fatalf("resolver calls %d rows %v key %x", res.count(), m.ms.dns, m.ms.links[odDomain].Key)
	}
}

// Taking Bedrock away (or saving Java only) is never refused and never asks
// DNS, even while every lookup fails.
func TestBedrockRemoveNeverResolvesAndIsNeverRefused(t *testing.T) {
	m, res := newBedrockOwnDomainEnv(t)
	res.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	m.ms.links[odDomain] = store.LinkRouteEdition{Domain: odDomain, OwnerID: "owner-1", Java: true, Bedrock: true, Key: make([]byte, 16)}
	if rec, _ := m.panelBedrock(odDomain, `{"java":true,"bedrock":false}`); rec.Code != 200 || m.ms.links[odDomain].Bedrock {
		t.Fatalf("remove %d %s", rec.Code, rec.Body.String())
	}
	seedLinkRoute(t, m.e.h.d.Redis, "fresh.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
	m.ms.core = append(m.ms.core, store.CoreLinkRoute{Domain: "fresh.example.com", OwnerID: "owner-1", LinkToken: "tok"})
	if rec, _ := m.panelBedrock("fresh.example.com", `{"java":true,"bedrock":false}`); rec.Code != 200 {
		t.Fatalf("java only %d %s", rec.Code, rec.Body.String())
	}
	if res.count() != 0 || len(m.ms.dns) != 0 {
		t.Fatalf("resolver calls %d rows %v", res.count(), m.ms.dns)
	}
}

// Review Focus 4: the route must be the caller's own before anything is
// resolved.
func TestPointingAdmissionZeroLookupsBedrock(t *testing.T) {
	cases := []struct {
		name, domain, user string
		setup              func(m *managedEnv)
	}{
		{"another account's route", odDomain, "owner-2", func(*managedEnv) {}},
		{"no route at all", "nope.example.com", "owner-1", func(*managedEnv) {}},
		{"only in Redis, not in core_link_routes", "redis-only.example.com", "owner-1", func(m *managedEnv) {
			seedLinkRoute(t, m.e.h.d.Redis, "redis-only.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, res := newBedrockOwnDomainEnv(t)
			for _, d := range []string{odDomain, "nope.example.com", "redis-only.example.com"} {
				res.a[d] = addrs(edgeAddr)
			}
			c.setup(m)
			rec := linkCall(m.e, m.e.h.SetLinkRouteBedrock, http.MethodPut, c.domain, bedrockOn, c.user, false)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d body %s, want 404", rec.Code, rec.Body.String())
			}
			if res.count() != 0 || len(m.ms.dns) != 0 || len(m.ms.links) != 0 {
				t.Fatalf("resolver calls %d rows %v links %v, want none", res.count(), m.ms.dns, m.ms.links)
			}
		})
	}
}

type bedrockListItem struct {
	Domain  string `json:"domain"`
	Bedrock struct {
		Refusal         string  `json:"refusal"`
		DNSStatus       string  `json:"dns_status"`
		DNSCheckedAt    *string `json:"dns_checked_at"`
		DNSFailingSince *string `json:"dns_failing_since"`
	} `json:"bedrock"`
}

func panelBedrockList(t *testing.T, m *managedEnv) map[string]bedrockListItem {
	t.Helper()
	rec := linkCall(m.e, m.e.h.ListLinkRoutes, http.MethodGet, "", "", "owner-1", false)
	var items []bedrockListItem
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &items) != nil {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	out := map[string]bedrockListItem{}
	for _, it := range items {
		out[it.Domain] = it
	}
	return out
}

// The list shows the stored check of each own domain, refuses only where the
// region it found has no entry, and shows nothing for a regional address or
// a never-checked one.
func TestLinkRouteBedrockListShowsStoredDNSState(t *testing.T) {
	m, res := newBedrockOwnDomainEnv(t)
	for _, d := range []string{"us.example.com", "new.example.com", linkDomain} {
		seedLinkRoute(t, m.e.h.d.Redis, d, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
	}
	if _, err := m.ms.RecordAddressDNSCheck(odDomain, "elsewhere", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ms.RecordAddressDNSCheck("us.example.com", "ok", "us", true); err != nil {
		t.Fatal(err)
	}
	// A stray row for a regional address is never shown.
	if _, err := m.ms.RecordAddressDNSCheck(linkDomain, "elsewhere", "", false); err != nil {
		t.Fatal(err)
	}
	got := panelBedrockList(t, m)
	od := got[odDomain].Bedrock
	if od.DNSStatus != "elsewhere" || od.DNSCheckedAt == nil || od.DNSFailingSince == nil || od.Refusal != "" {
		t.Fatalf("%s %+v", odDomain, od)
	}
	if us := got["us.example.com"].Bedrock; us.DNSStatus != "ok" || us.DNSFailingSince != nil || us.Refusal != bedrockNoEntry {
		t.Fatalf("us.example.com %+v", us)
	}
	for _, d := range []string{"new.example.com", linkDomain} {
		if b := got[d].Bedrock; b.DNSStatus != "" || b.DNSCheckedAt != nil || b.DNSFailingSince != nil || b.Refusal != "" {
			t.Fatalf("%s %+v", d, b)
		}
	}
	if res.count() != 0 {
		t.Fatalf("the list made %d resolver calls", res.count())
	}
}

// No read path resolves: the panel and external route lists and the Floodgate
// key download, with an own-domain Bedrock row and a stored check present.
func TestPointingNeverResolvesOnReadPathsBedrock(t *testing.T) {
	m, res := newBedrockOwnDomainEnv(t)
	res.a[odDomain] = addrs(edgeAddr)
	m.ms.links[odDomain] = store.LinkRouteEdition{Domain: odDomain, OwnerID: "owner-1", Java: true, Bedrock: true, Key: make([]byte, 16)}
	if _, err := m.ms.RecordAddressDNSCheck(odDomain, "ok", "eu", true); err != nil {
		t.Fatal(err)
	}
	if got := panelBedrockList(t, m)[odDomain].Bedrock; got.DNSStatus != "ok" {
		t.Fatalf("panel list %+v", got)
	}
	ext := httptest.NewRecorder()
	m.x.ListRoutes(ext, extReq("GET", "/", "owner-1", false, nil, nil))
	if ext.Code != 200 || !strings.Contains(ext.Body.String(), `"dns_status":"ok"`) {
		t.Fatalf("external list %d %s", ext.Code, ext.Body.String())
	}
	if dl := linkCall(m.e, m.e.h.GetLinkRouteFloodgateKey, http.MethodGet, odDomain, "", "owner-1", false); dl.Code != 200 {
		t.Fatalf("key download %d", dl.Code)
	}
	if res.count() != 0 {
		t.Fatalf("resolver calls %d, want 0", res.count())
	}
}

// An own-domain Bedrock row that exists today (and a regional one) publishes
// as before and lists as before; the only additions are the three DNS keys,
// empty until a check ran. No lookup anywhere.
func TestExistingCustomersSeeNoChangeUntilTheyAddSomethingBedrock(t *testing.T) {
	m, res := newBedrockOwnDomainEnv(t)
	seedLinkRoute(t, m.e.h.d.Redis, linkDomain, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
	key := bytes.Repeat([]byte{9}, 16)
	for _, d := range []string{odDomain, linkDomain} {
		m.ms.links[d] = store.LinkRouteEdition{Domain: d, OwnerID: "owner-1", Java: true, Bedrock: true, Key: key}
	}
	if err := m.e.h.bedrockService().PublishEditions(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{odDomain, linkDomain} {
		if got := m.e.mr.HGet(services.RouteEditionsKey, d); got != "both" {
			t.Fatalf("%s edition %q, want both", d, got)
		}
		pub, _ := m.e.mr.Get(services.FloodgateKeyRedisKey("owner:owner-1:bedrock:" + d))
		if pub != base64.StdEncoding.EncodeToString(key) {
			t.Fatalf("%s floodgate key %q", d, pub)
		}
	}
	rec := linkCall(m.e, m.e.h.ListLinkRoutes, http.MethodGet, "", "", "owner-1", false)
	var items []map[string]json.RawMessage
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &items) != nil {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	const want = `{"java":true,"bedrock":true,"has_key":true,"dns_status":"","dns_checked_at":null,"dns_failing_since":null}`
	seen := 0
	for _, it := range items {
		var d string
		_ = json.Unmarshal(it["domain"], &d)
		if d == odDomain || d == linkDomain {
			seen++
			if string(it["bedrock"]) != want {
				t.Fatalf("%s bedrock %s, want %s", d, it["bedrock"], want)
			}
		}
	}
	if seen != 2 || res.count() != 0 {
		t.Fatalf("saw %d rows, resolver calls %d", seen, res.count())
	}
}
