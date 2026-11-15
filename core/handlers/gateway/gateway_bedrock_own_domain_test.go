package gateway

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/store"
)

// --- Hosted Bedrock per address (local mode). ---

// newHostedBedrock is newLinkBedrock (Bedrock for everyone, the eu entry, a
// store with address_dns_checks) for server srv-7 whose only address is
// odDomain, the eu edge at edgeAddr, and the shared checker over a fake
// resolver.
func newHostedBedrock(t *testing.T) (*voiceSwitchEnv, *linkBedrockStore, *odResolver) {
	t.Helper()
	e, ls := newLinkBedrock(t)
	e.mr.Del("route:a.eu.dylaris.com")
	e.mr.SRem("sys:index:routes", "a.eu.dylaris.com")
	e.addHostedRoute(odDomain)
	e.mr.Set("edge:registry:e1", `{"edge_id":"e1","status":"online","region":"eu","wildcard":"*.eu.dylaris.com","ip":"`+edgeAddr+`"}`)
	res := &odResolver{cname: map[string]string{}, a: map[string][]netip.Addr{}, srv: map[string][]*net.SRV{}}
	e.h.d.Pointing = odChecker(e.h.d.Redis, res)
	return e, ls, res
}

func hostedEdition(domain string, java, bedrock bool) string {
	b, _ := json.Marshal(map[string]interface{}{"domain": domain, "java": java, "bedrock": bedrock})
	return string(b)
}

func hostedView(t *testing.T, e *voiceSwitchEnv) map[string]interface{} {
	t.Helper()
	rec, out := bedrockCall(e, http.MethodGet, "", false)
	v, ok := out["bedrock"].(map[string]interface{})
	if rec.Code != http.StatusOK || !ok {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	return v
}

// The add is decided by the address being added: its own region's entry for
// one of the gateway's domains, the DNS check for an own domain.
func TestHostedBedrockPerAddress(t *testing.T) {
	elsewhere := odDomain + " points to " + foreignAddr + ", not to the gateway. Add a CNAME from " + odDomain + " to the address shown under Your own domains. " +
		"A proxy or DDoS filter in front of the domain cannot carry Bedrock."
	cases := []struct {
		name    string
		setup   func(e *voiceSwitchEnv, r *odResolver)
		domain  string
		status  int
		code    string
		message string
		stored  services.PointingStatus // "" = no row
	}{
		{"own domain ok with entry", func(_ *voiceSwitchEnv, r *odResolver) { r.a[odDomain] = addrs(edgeAddr) },
			odDomain, 200, "", "", services.PointingOK},
		{"own domain ok, no entry in its region", func(e *voiceSwitchEnv, r *odResolver) {
			r.a[odDomain] = addrs(edgeAddr)
			e.mr.Set("bedrock:registry:b1", `{"id":"b1","region":"us","version":"1"}`)
		}, odDomain, 409, "bedrock_unavailable", bedrockNoEntry, services.PointingOK},
		{"own domain elsewhere", func(_ *voiceSwitchEnv, r *odResolver) { r.a[odDomain] = addrs(foreignAddr) },
			odDomain, 409, "bedrock_unavailable", elsewhere, services.PointingElsewhere},
		{"own domain lookup failed", func(_ *voiceSwitchEnv, r *odResolver) {
			r.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		}, odDomain, 503, "dns_lookup_failed", "The lookup of " + odDomain + " failed right now. Nothing was changed. Try again in a moment.", ""},
		{"no checker", func(e *voiceSwitchEnv, _ *odResolver) { e.h.d.Pointing = nil },
			odDomain, 503, "dns_lookup_failed", "The lookup of " + odDomain + " failed right now. Nothing was changed. Try again in a moment.", ""},
		// Before, the server's pick (a.eu, no route here: the first regional
		// route) decided for every address; now b.us's own region does.
		{"regional second route, its region has no entry", func(e *voiceSwitchEnv, _ *odResolver) {
			e.addHostedRoute("a.eu.dylaris.com")
			e.addHostedRoute("b.us.dylaris.com")
			e.mr.Set("edge:registry:e2", `{"edge_id":"e2","status":"online","region":"us","wildcard":"*.us.dylaris.com","ip":"`+usEdgeAddr+`"}`)
		}, "b.us.dylaris.com", 409, "bedrock_unavailable", bedrockNoEntry, ""},
		{"regional second route, its region has the entry", func(e *voiceSwitchEnv, _ *odResolver) {
			e.addHostedRoute("a.eu.dylaris.com")
			e.addHostedRoute("b.us.dylaris.com")
			e.mr.Set("edge:registry:e2", `{"edge_id":"e2","status":"online","region":"us","wildcard":"*.us.dylaris.com","ip":"`+usEdgeAddr+`"}`)
			e.mr.Set("bedrock:registry:b1", `{"id":"b1","region":"us","version":"1"}`)
		}, "b.us.dylaris.com", 200, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, ls, res := newHostedBedrock(t)
			c.setup(e, res)
			rec, out := bedrockCall(e, http.MethodPut, hostedEdition(c.domain, true, true), false)
			if rec.Code != c.status {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), c.status)
			}
			row, stored := ls.dns[c.domain]
			if c.stored == "" {
				if stored {
					t.Fatalf("stored %+v, want no row", row)
				}
			} else if !stored || row.Status != string(c.stored) {
				t.Fatalf("stored %+v (%v), want %s", row, stored, c.stored)
			}
			if c.status != 200 {
				if out["code"] != c.code || out["message"] != c.message {
					t.Fatalf("answer %v, want %s %q", out, c.code, c.message)
				}
				if len(ls.eds) != 0 || ls.key != nil || e.mr.Exists(services.RouteEditionsKey) {
					t.Fatal("a refused add saved something")
				}
				return
			}
			if got := e.mr.HGet(services.RouteEditionsKey, c.domain); got != "both" {
				t.Fatalf("edition %q, want both", got)
			}
		})
	}

	t.Run("rate limited", func(t *testing.T) {
		e, ls, res := newHostedBedrock(t)
		res.a[odDomain] = addrs(edgeAddr)
		e.h.d.PointingLimiter = httpx.NewIPRateLimiter()
		for i := 0; i < pointingChecksPerMin; i++ {
			e.h.d.PointingLimiter.Allow("ip:192.0.2.1", pointingChecksPerMin)
		}
		rec, out := bedrockCall(e, http.MethodPut, hostedEdition(odDomain, true, true), false)
		if rec.Code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
			t.Fatalf("%d %v", rec.Code, out)
		}
		if res.count() != 0 || len(ls.dns) != 0 || len(ls.eds) != 0 {
			t.Fatalf("resolver calls %d rows %v editions %v", res.count(), ls.dns, ls.eds)
		}
	})
}

// Review Focus 3: the view, a removal and a re-assert of an address that has
// Bedrock make no lookup, whatever its DNS says now.
func TestHostedBedrockViewNeverResolves(t *testing.T) {
	e, ls, res := newHostedBedrock(t)
	res.a[odDomain] = addrs(foreignAddr)
	ls.dns[odDomain] = store.AddressDNSCheck{Name: odDomain, Status: "elsewhere", CheckedAt: time.Now()}
	v := hostedView(t, e)
	routes, _ := v["routes"].([]interface{})
	if len(routes) != 1 {
		t.Fatalf("routes %v", routes)
	}
	if rt := routes[0].(map[string]interface{}); rt["dnsStatus"] != "elsewhere" || rt["dnsCheckedAt"] == nil || rt["ownDomain"] != true {
		t.Fatalf("route %v", rt)
	}
	ls.eds[odDomain] = store.RouteEdition{Domain: odDomain, ServerID: 7, ServerUUID: "srv-7", Java: true, Bedrock: true}
	for _, body := range []string{hostedEdition(odDomain, false, true), hostedEdition(odDomain, true, true), hostedEdition(odDomain, true, false)} {
		if rec, _ := bedrockCall(e, http.MethodPut, body, false); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	hostedView(t, e)
	if res.count() != 0 {
		t.Fatalf("resolver calls %d, want 0", res.count())
	}
}

// A server whose only address is its own domain is offered Bedrock when any
// region runs an entry; the address it shows is that domain.
func TestHostedBedrockOwnDomainOnlyServerIsOffered(t *testing.T) {
	e, _, res := newHostedBedrock(t)
	v := hostedView(t, e)
	routes, _ := v["routes"].([]interface{})
	if v["available"] != true || v["address"] != odDomain || v["refusal"] != nil || len(routes) != 1 {
		t.Fatalf("view %v", v)
	}
	rt := routes[0].(map[string]interface{})
	if rt["domain"] != odDomain || rt["java"] != true || rt["bedrock"] != false || rt["ownDomain"] != true {
		t.Fatalf("route %v", rt)
	}
	if _, has := rt["dnsStatus"]; has {
		t.Fatalf("never checked, but dnsStatus %v", rt["dnsStatus"])
	}

	e.mr.Del("bedrock:registry:b1")
	if v := hostedView(t, e); v["available"] != false || v["refusal"] != bedrockNoEntry || v["address"] != odDomain {
		t.Fatalf("no entry anywhere: %v", v)
	}
	e.mr.Del("route:" + odDomain)
	if v := hostedView(t, e); v["available"] != false || v["refusal"] != bedrockNotRouted {
		t.Fatalf("no route: %v", v)
	}
	if res.count() != 0 {
		t.Fatalf("resolver calls %d", res.count())
	}
}

// An own-domain Bedrock row added before this release stays and is
// published; GET lists it without a DNS state until the first re-check, and
// nothing resolves.
func TestExistingCustomersSeeNoChangeUntilTheyAddSomethingHostedBedrock(t *testing.T) {
	e, ls, res := newHostedBedrock(t)
	ls.key = bytes.Repeat([]byte{7}, 16)
	ls.eds[odDomain] = store.RouteEdition{Domain: odDomain, ServerID: 7, ServerUUID: "srv-7", Java: true, Bedrock: true}
	if err := e.h.bedrockService().PublishEditions(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if got := e.mr.HGet(services.RouteEditionsKey, odDomain); got != "both" {
		t.Fatalf("edition %q, want both", got)
	}
	rec, _ := bedrockCall(e, http.MethodGet, "", false)
	const want = `"routes":[{"domain":"play.example.com","java":true,"bedrock":true,"ownDomain":true}]`
	if !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("get %s, want %s", rec.Body.String(), want)
	}
	if res.count() != 0 || len(ls.dns) != 0 {
		t.Fatalf("resolver calls %d rows %v", res.count(), ls.dns)
	}
}
