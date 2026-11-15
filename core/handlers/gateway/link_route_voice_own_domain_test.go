package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

const (
	odDomain    = "play.example.com"
	ourVoice    = "voice.eu.dylaris.com"
	edgeAddr    = "203.0.113.10"
	foreignAddr = "198.51.100.7"
)

// odResolver is the DNS behind the shared pointing checker in these tests;
// calls counts every lookup so a test can say "none were made".
type odResolver struct {
	mu    sync.Mutex
	cname map[string]string
	a     map[string][]netip.Addr
	srv   map[string][]*net.SRV
	fail  error
	calls int
	// block, when set, holds every lookup until it closes or ctx ends.
	block chan struct{}
}

var odNotFound = &net.DNSError{Err: "no such host", IsNotFound: true}

func (f *odResolver) enter(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	fail, block := f.fail, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fail
}
func (f *odResolver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
func (f *odResolver) LookupCNAME(ctx context.Context, h string) (string, error) {
	if err := f.enter(ctx); err != nil {
		return "", err
	}
	if c, ok := f.cname[h]; ok {
		return c + ".", nil
	}
	if len(f.a[h]) > 0 {
		return h + ".", nil
	}
	return "", odNotFound
}
func (f *odResolver) LookupA(ctx context.Context, h string) ([]netip.Addr, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	if c, ok := f.cname[h]; ok {
		h = c
	}
	if v := f.a[h]; len(v) > 0 {
		return v, nil
	}
	return nil, odNotFound
}
func (f *odResolver) LookupAAAA(ctx context.Context, _ string) ([]netip.Addr, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	return nil, odNotFound
}
func (f *odResolver) LookupSRV(ctx context.Context, h string) ([]*net.SRV, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	if v := f.srv[h]; len(v) > 0 {
		return v, nil
	}
	return nil, odNotFound
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(s))
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

// newOwnDomainEnv is newManagedEnv with the global voice switch on (so the
// panel may enable it too), the eu edge at edgeAddr, play.example.com live in
// Redis as owner-1's route, and the production pointing checker wired to a
// fake resolver.
func newOwnDomainEnv(t *testing.T) (*managedEnv, *odResolver) {
	t.Helper()
	m := newManagedEnv(t, true)
	m.ms.global = "true"
	m.e.mr.Set("edge:registry:e1", `{"edge_id":"e1","status":"online","region":"eu","wildcard":"*.eu.dylaris.com","ip":"`+edgeAddr+`"}`)
	seedLinkRoute(t, m.e.h.d.Redis, odDomain, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
	res := &odResolver{cname: map[string]string{}, a: map[string][]netip.Addr{}, srv: map[string][]*net.SRV{}}
	m.e.h.d.Pointing = odChecker(m.e.h.d.Redis, res)
	return m, res
}

// odChecker is main.go's wiring over res; a fresh one has an empty cache.
func odChecker(rdb *redis.Client, res *odResolver) *services.PointingChecker {
	chk := services.NewPointingChecker(res, func(ctx context.Context) []services.GatewayEdgeInfo {
		return services.GatewayEdgeRegistry(ctx, rdb)
	})
	chk.SetAdmitted(func(ctx context.Context, name string) (bool, error) {
		return services.GatewayRouteCovers(ctx, rdb, name)
	})
	return chk
}

func (m *managedEnv) panelVoice(domain, user string) (*httptest.ResponseRecorder, map[string]interface{}) {
	rec := linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, domain, `{"enabled":true}`, user, false)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (m *managedEnv) voicePortKeys() []string {
	var out []string
	for _, k := range m.e.mr.Keys() {
		if strings.HasPrefix(k, "voice:port:") {
			out = append(out, k)
		}
	}
	return out
}

func notPointingText(name string) string {
	return "Voice chat needs to know which region this server's players arrive in. " + name +
		" does not point at us yet: add a CNAME from " + name + " to the address shown under Your own domains, or add an address on one of our domains."
}

func TestLinkRouteVoiceOwnDomain(t *testing.T) {
	cases := []struct {
		name    string
		dns     func(r *odResolver)
		status  int
		code    string
		message string
	}{
		{"ok by cname", func(r *odResolver) {
			r.cname[odDomain] = "route.eu.dylaris.com"
			r.a["route.eu.dylaris.com"] = addrs(edgeAddr)
		}, 200, "", ""},
		{"ok by a", func(r *odResolver) { r.a[odDomain] = addrs(edgeAddr) }, 200, "", ""},
		{"ok by srv", func(r *odResolver) {
			r.srv["_minecraft._tcp."+odDomain] = []*net.SRV{{Target: "route.eu.dylaris.com.", Port: 25565}}
		}, 200, "", ""},
		{"elsewhere", func(r *odResolver) { r.a[odDomain] = addrs(foreignAddr) }, 409, "no_routes", notPointingText(odDomain)},
		{"not found", func(*odResolver) {}, 409, "no_routes", notPointingText(odDomain)},
		{"partial", func(r *odResolver) { r.a[odDomain] = addrs(edgeAddr, foreignAddr) }, 409, "no_routes",
			odDomain + " points to us and also to " + foreignAddr + ". Remove the records that are not ours, or use a CNAME to the address shown under Your own domains."},
		{"private", func(r *odResolver) { r.a[odDomain] = addrs("10.0.0.5") }, 409, "no_routes",
			odDomain + " resolves to an address that is not on the public internet."},
		{"lookup failed", func(r *odResolver) { r.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true} }, 503, "dns_lookup_failed",
			"We could not look up " + odDomain + " right now. Nothing was changed. Try again in a moment."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, res := newOwnDomainEnv(t)
			c.dns(res)
			rec, out := m.panelVoice(odDomain, "owner-1")
			if rec.Code != c.status {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), c.status)
			}
			if c.status != 200 {
				if out["code"] != c.code || out["message"] != c.message {
					t.Fatalf("answer %v, want %s %q", out, c.code, c.message)
				}
				if len(m.ms.voice) != 0 || len(m.voicePortKeys()) != 0 {
					t.Fatalf("a refusal wrote something: rows %v keys %v", m.ms.voice, m.voicePortKeys())
				}
				return
			}
			v := out["voice"].(map[string]interface{})
			if v["host"] != ourVoice || m.ms.voice[odDomain].Host != ourVoice || m.ms.voice[odDomain].Region != "eu" {
				t.Fatalf("answer %v row %+v, want host %s in eu", v, m.ms.voice[odDomain], ourVoice)
			}
			// The relay's entry stays keyed by the ROUTE domain.
			got, _ := m.e.mr.Get("voice:port:eu:" + strconv.Itoa(services.VoicePortMin))
			if !strings.Contains(got, `"server_uuid":"owner:owner-1:voice:`+odDomain+`"`) {
				t.Fatalf("relay entry %s", got)
			}
		})
	}

	t.Run("rate limited", func(t *testing.T) {
		m, res := newOwnDomainEnv(t)
		res.a[odDomain] = addrs(edgeAddr)
		m.e.h.d.PointingLimiter = httpx.NewIPRateLimiter()
		for i := 0; i < pointingChecksPerMin; i++ {
			m.e.h.d.PointingLimiter.Allow("user:owner-1", pointingChecksPerMin)
		}
		rec, out := m.panelVoice(odDomain, "owner-1")
		if rec.Code != http.StatusTooManyRequests || out["code"] != "rate_limited" || out["message"] != "Too many DNS checks. Wait a minute." {
			t.Fatalf("%d %v", rec.Code, out)
		}
		if res.count() != 0 || len(m.ms.voice) != 0 {
			t.Fatalf("resolver calls %d rows %v", res.count(), m.ms.voice)
		}
	})

	t.Run("no checker is could not check", func(t *testing.T) {
		m, _ := newOwnDomainEnv(t)
		m.e.h.d.Pointing = nil
		if rec, out := m.panelVoice(odDomain, "owner-1"); rec.Code != 503 || out["code"] != "dns_lookup_failed" {
			t.Fatalf("%d %v", rec.Code, out)
		}
	})

	// The external API passes the retryable code through instead of masking
	// it as internal, and spends no panel DNS budget.
	t.Run("external lookup failed", func(t *testing.T) {
		m, res := newOwnDomainEnv(t)
		res.fail = &net.DNSError{Err: "i/o timeout", IsTimeout: true}
		rec := httptest.NewRecorder()
		m.x.SetRouteVoice(rec, extReq("PUT", "/", "owner-1", false, map[string]string{"domain": odDomain}, map[string]interface{}{"enabled": true}))
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 503 || out["code"] != "dns_lookup_failed" || out["success"] != false ||
			out["error"] != "We could not look up "+odDomain+" right now. Nothing was changed. Try again in a moment." {
			t.Fatalf("%d %v", rec.Code, out)
		}
	})
}

// m2 (review 3-4): the per-owner DNS bound is the ROUTE owner's, not the
// caller's. owner-1 already has the most lookups running; an admin enabling
// owner-1's address gets 503 dns_lookup_failed at once and starts no lookup.
// Passing the caller's id or "" instead would start one and wait on it.
func TestLinkRouteVoiceOwnDomainCapIsTheRouteOwners(t *testing.T) {
	m, res := newOwnDomainEnv(t)
	release := make(chan struct{})
	res.block = release
	busy := []string{"b1.example.com", "b2.example.com"}
	var wg sync.WaitGroup
	for _, d := range busy {
		seedLinkRoute(t, m.e.h.d.Redis, d, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			m.e.h.d.Pointing.CheckFor(context.Background(), "owner-1", d, services.PurposeVoiceRegion, nil)
		}(d)
	}
	defer func() { close(release); wg.Wait() }()
	// Each running lookup asks CNAME, A and AAAA at once.
	for start := time.Now(); res.count() < 3*len(busy); {
		if time.Since(start) > 2*time.Second {
			t.Fatalf("only %d lookups started", res.count())
		}
		time.Sleep(time.Millisecond)
	}
	before := res.count()
	start := time.Now()
	rec, out := linkCallJSON(m, odDomain, "admin-9", true)
	if rec.Code != http.StatusServiceUnavailable || out["code"] != "dns_lookup_failed" || time.Since(start) > time.Second {
		t.Fatalf("%d %v after %v, want 503 dns_lookup_failed at once", rec.Code, out, time.Since(start))
	}
	if n := res.count() - before; n != 0 || len(m.ms.voice) != 0 {
		t.Fatalf("%d lookups started for a capped owner, rows %v", n, m.ms.voice)
	}
}

func linkCallJSON(m *managedEnv, domain, user string, admin bool) (*httptest.ResponseRecorder, map[string]interface{}) {
	rec := linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, domain, `{"enabled":true}`, user, admin)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// Review Focus 5: an address that already has voice keeps its region and
// port when it is enabled again, without asking DNS, even when its DNS now
// points elsewhere. This is what keeps the 7b-2 sweep's re-assert away from DNS.
func TestOwnDomainVoiceReassertKeepsRegionWithoutDNS(t *testing.T) {
	m, res := newOwnDomainEnv(t)
	res.a[odDomain] = addrs(edgeAddr)
	if rec, _ := m.panelVoice(odDomain, "owner-1"); rec.Code != 200 {
		t.Fatalf("first enable %d %s", rec.Code, rec.Body.String())
	}
	first := m.ms.voice[odDomain]
	res.a[odDomain] = addrs(foreignAddr)
	// A fresh checker, so a cached ok cannot hide a lookup.
	m.e.h.d.Pointing = odChecker(m.e.h.d.Redis, res)
	before := res.count()
	for _, call := range []func() int{
		func() int { rec, _ := m.panelVoice(odDomain, "owner-1"); return rec.Code },
		func() int { status, _ := m.extVoiceOn(odDomain); return status },
	} {
		if code := call(); code != 200 {
			t.Fatalf("re-enable %d", code)
		}
		got := m.ms.voice[odDomain]
		if got.Region != first.Region || got.Port != first.Port || got.Host != ourVoice {
			t.Fatalf("re-enable = %+v, first %+v", got, first)
		}
	}
	if n := res.count() - before; n != 0 {
		t.Fatalf("re-enable made %d resolver calls, want 0", n)
	}
}

func (m *managedEnv) extVoiceOn(domain string) (int, map[string]interface{}) {
	rec := httptest.NewRecorder()
	m.x.SetRouteVoice(rec, extReq("PUT", "/", "owner-1", false, map[string]string{"domain": domain}, map[string]interface{}{"enabled": true}))
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// Review Focus 4: the route must be the caller's own before anything is
// resolved. Each of these answers before the checker is asked.
func TestPointingAdmissionZeroLookups(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(m *managedEnv)
		domain string
		user   string
		admin  bool
		status int
	}{
		{"another account's route", func(*managedEnv) {}, odDomain, "owner-2", false, 404},
		{"no route at all", func(*managedEnv) {}, "nope.example.com", "owner-1", false, 404},
		{"admin, no route at all", func(*managedEnv) {}, "nope.example.com", "admin-1", true, 404},
		{"only in Redis, not in core_link_routes", func(m *managedEnv) {
			seedLinkRoute(t, m.e.h.d.Redis, "redis-only.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
		}, "redis-only.example.com", "owner-1", false, 404},
		{"route table unreadable", func(m *managedEnv) { m.ms.linkBedrockStore.core = nil; m.e.h.d.Store = &coreErrStore{m.ms} },
			odDomain, "owner-1", false, 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, res := newOwnDomainEnv(t)
			res.a[odDomain] = addrs(edgeAddr)
			res.a["nope.example.com"] = addrs(edgeAddr)
			res.a["redis-only.example.com"] = addrs(edgeAddr)
			c.setup(m)
			rec := linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, c.domain, `{"enabled":true}`, c.user, c.admin)
			if rec.Code != c.status {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), c.status)
			}
			if res.count() != 0 || len(m.ms.voice) != 0 {
				t.Fatalf("resolver calls %d rows %v, want none", res.count(), m.ms.voice)
			}
		})
	}
}

// coreErrStore fails the route table read, as a database fault does.
type coreErrStore struct{ *managedStore }

func (coreErrStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return nil, errors.New("connection reset by peer")
}

// The link resolves OUR name for an own-domain row and the route domain for
// every other row, byte for byte as before; the subject is the route domain
// either way, which is the relay's identity.
func TestLinkVoiceCarriesOurNameForOwnDomains(t *testing.T) {
	h, fs, key, token := routeOnlyKit(t)
	k := strings.Repeat("11", 32)
	raw, _ := hex.DecodeString(k)
	fs.voice = []store.LinkRouteVoice{
		{Domain: "a.eu.dylaris.com", OwnerID: "owner-1", LinkToken: token, TargetHost: "127.0.0.1", Port: 26001, TargetPort: 24454, UplinkKey: raw},
		{Domain: odDomain, OwnerID: "owner-1", LinkToken: token, TargetHost: "127.0.0.1", Port: 26002, TargetPort: 24455, UplinkKey: raw, Host: ourVoice},
	}
	rec := callAs(h.LinkVoice, http.MethodGet, "/api/warp/link/voice", "", key)
	var out struct {
		Routes json.RawMessage `json:"routes"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	want := `[{"domain":"a.eu.dylaris.com","subject":"owner:owner-1:voice:a.eu.dylaris.com","port":26001,"key":"` + k + `","target":"127.0.0.1:24454"},` +
		`{"domain":"` + ourVoice + `","subject":"owner:owner-1:voice:` + odDomain + `","port":26002,"key":"` + k + `","target":"127.0.0.1:24455"}]`
	if string(out.Routes) != want {
		t.Fatalf("routes\n got %s\nwant %s", out.Routes, want)
	}
}

// A regional address with voice today: enabling it again, listing it and the
// relay entry are what they were, with no lookup. The one addition is host,
// equal to the address itself.
func TestExistingCustomersSeeNoChangeUntilTheyAddSomething(t *testing.T) {
	t.Run("route-only", existingRouteOnlyCustomerSeesNoChange)
	t.Run("hosted", existingHostedCustomerSeesNoChange)
}

func existingRouteOnlyCustomerSeesNoChange(t *testing.T) {
	m, res := newOwnDomainEnv(t)
	seedLinkRoute(t, m.e.h.d.Redis, linkDomain, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
	m.ms.voice[linkDomain] = store.LinkRouteVoice{Domain: linkDomain, OwnerID: "owner-1", LinkToken: "tok", TargetHost: "127.0.0.1",
		Region: "eu", Port: services.VoicePortMin, TargetPort: 24454, UplinkKey: make([]byte, 32)}

	rec, _ := m.panelVoice(linkDomain, "owner-1")
	port := strconv.Itoa(services.VoicePortMin)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"success":true,"voice":{"enabled":true,"host":"`+linkDomain+`","port":`+port+`,"target_port":24454}}` {
		t.Fatalf("enable answer %s", got)
	}
	row := m.ms.voice[linkDomain]
	if row.Host != "" || row.Region != "eu" {
		t.Fatalf("row %+v, want host '' in eu", row)
	}
	entry, _ := m.e.mr.Get("voice:port:eu:" + port)
	if entry != `{"v":1,"server_uuid":"owner:owner-1:voice:`+linkDomain+`","key":"`+hex.EncodeToString(row.UplinkKey)+`"}` {
		t.Fatalf("relay entry %s", entry)
	}
	list := linkCall(m.e, m.e.h.ListLinkRoutes, http.MethodGet, "", "", "owner-1", false)
	if !strings.Contains(list.Body.String(), `"voice":{"enabled":true,"host":"`+linkDomain+`","port":`+port+`,"target_port":24454}`) {
		t.Fatalf("list %s", list.Body.String())
	}
	if res.count() != 0 {
		t.Fatalf("resolver calls %d, want 0", res.count())
	}
}

// No read path resolves: the panel and external route lists and the link's
// voice list, with an own-domain row present.
func TestPointingNeverResolvesOnReadPaths(t *testing.T) {
	m, res := newOwnDomainEnv(t)
	m.ms.voice[odDomain] = store.LinkRouteVoice{Domain: odDomain, OwnerID: "owner-1", LinkToken: "tok", TargetHost: "127.0.0.1",
		Region: "eu", Port: services.VoicePortMin, TargetPort: 24454, UplinkKey: make([]byte, 32), Host: ourVoice}
	list := linkCall(m.e, m.e.h.ListLinkRoutes, http.MethodGet, "", "", "owner-1", false)
	ext := httptest.NewRecorder()
	m.x.ListRoutes(ext, extReq("GET", "/", "owner-1", false, nil, nil))
	for name, body := range map[string]string{"panel list": list.Body.String(), "external list": ext.Body.String()} {
		if !strings.Contains(body, `"host":"`+ourVoice+`"`) {
			t.Fatalf("%s does not carry our name: %s", name, body)
		}
	}
	h, fs, key, token := routeOnlyKit(t)
	fs.voice = []store.LinkRouteVoice{{Domain: odDomain, OwnerID: "owner-1", LinkToken: token, TargetHost: "127.0.0.1", Port: 26001, TargetPort: 24454, UplinkKey: make([]byte, 32), Host: ourVoice}}
	if rec := callAs(h.LinkVoice, http.MethodGet, "/api/warp/link/voice", "", key); rec.Code != 200 {
		t.Fatalf("link voice %d", rec.Code)
	}
	if res.count() != 0 {
		t.Fatalf("resolver calls %d, want 0", res.count())
	}
}

// An old customer Core decodes the new answer: host is an extra key its
// RouteVoice ignores.
func TestOldClientParsesVoiceAnswerWithHost(t *testing.T) {
	m, res := newOwnDomainEnv(t)
	res.a[odDomain] = addrs(edgeAddr)
	status, out := m.extVoiceOn(odDomain)
	if status != 200 {
		t.Fatalf("%d %v", status, out)
	}
	raw, _ := json.Marshal(out["voice"])
	var v remotegw.RouteVoice
	if err := json.Unmarshal(raw, &v); err != nil || !v.Enabled || v.Port != services.VoicePortMin || v.TargetPort != 24454 {
		t.Fatalf("decoded %+v, %v from %s", v, err, raw)
	}
}
