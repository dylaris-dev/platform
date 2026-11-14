// Package remotegwtest holds the in-memory gateway provider for tests. It is
// a normal package so other packages' tests can share it, and it imports
// testing, so only _test.go files may import it (layering_test.go enforces
// that, and TestProductionBinaryDoesNotLinkTesting keeps it out of the binary).
package remotegwtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-pkg/validate"
)

// apiPrefix is the client's (unexported) path prefix; a drift fails every
// client test against the fake with a 404.
const apiPrefix = "/api/external/gateway"

type fakeKit struct {
	id     int
	name   string
	linkID string
	region string
	online bool
	caps   []string
	retire *time.Time
}

type fakeRoute struct {
	domain, linkID, host string
	port                 int
	proxy                bool
}

// fakeDomain is a provider claim of the fake's one account.
type fakeDomain struct {
	domain, state, token string
	attempts             int
	// lapsesAt is set on a verified claim whose record the provider's
	// verifier stopped finding (the real FailingSince + grace).
	lapsesAt *time.Time
}

// fakeHoster is the fake's one hoster domain, as /regions lists it.
const fakeHoster = "example.com"

// dnsLabel and isHostname mirror handlers/validate.IsHostname, which a services
// package may not import.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func isHostname(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if len(l) > 63 || !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}

func underHoster(d string) bool { return d == fakeHoster || strings.HasSuffix(d, "."+fakeHoster) }

type failure struct {
	status     int
	code       string
	retryAfter time.Duration
}

// Fake is an in-memory provider. It implements the happy paths and the
// refusals the customer side acts on; anything else is a FailNext away.
type Fake struct {
	srv *httptest.Server

	mu     sync.Mutex
	key    string
	kits   []*fakeKit
	routes []*fakeRoute
	nextID int
	fails  []failure
	calls  []string
	acct   *remotegw.Account // nil: the default managed account
	// linkLimit caps live (not retiring) kits like the plan's maxLinks; nil
	// mints without a cap and refuses every retirement as not needed.
	linkLimit *int
	// retireBudget is the retirements allowed in all; nil is 4 per host.
	retireBudget *int
	retired      int
	// loseNext: the next request is carried out, then answered 502, as if
	// the answer was lost on its way back.
	loseNext bool

	domains []*fakeDomain
	// customOff refuses own domains like a provider with them switched off.
	customOff bool
	// published: the domains whose provider TXT value is in DNS.
	published map[string]bool
	// checked: the last manual check per domain, for the 5-minute cooldown.
	checked map[string]time.Time
	// liveReverify: the route gate proves a pending or blocked claim whose TXT
	// value is published on the spot, as the real gate does.
	liveReverify bool
}

// fakeClaimCap is the provider's per-account claim bound (maxClaimsPerUser).
const fakeClaimCap = 50

// fakeVerifyCooldown is the provider's verifyCheckCooldown.
const fakeVerifyCooldown = 5 * time.Minute

func NewFake(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{key: "dyl_fake_provider_key", nextID: 1, published: map[string]bool{}, checked: map[string]time.Time{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *Fake) URL() string { return f.srv.URL }

func (f *Fake) Key() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key
}

// SetKey changes the key the fake accepts, like a key rolled on the provider.
func (f *Fake) SetKey(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key = k
}

// SetAccount makes GET /account answer a, e.g. a plan that is not managed.
func (f *Fake) SetAccount(a remotegw.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acct = &a
}

// SetLinkLimit caps live kits at n, as a plan's host limit does.
func (f *Fake) SetLinkLimit(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkLimit = &n
}

// SetRetireBudget sets how many retirements the fake allows in all.
func (f *Fake) SetRetireBudget(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retireBudget = &n
}

// LoseNextAnswer carries out the next request but answers it 502.
func (f *Fake) LoseNextAnswer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseNext = true
}

// FailNext makes the next n requests fail with status. An empty code answers
// in the auth layer's shape (message, no code), else in the handlers' shape.
func (f *Fake) FailNext(n int, status int, code string, retryAfter time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < n; i++ {
		f.fails = append(f.fails, failure{status, code, retryAfter})
	}
}

// FailAfter lets the next ok requests through, then fails n like FailNext.
func (f *Fake) FailAfter(ok, n int, status int, code string, retryAfter time.Duration) {
	f.mu.Lock()
	for i := 0; i < ok; i++ {
		f.fails = append(f.fails, failure{})
	}
	f.mu.Unlock()
	f.FailNext(n, status, code, retryAfter)
}

// AddRoute puts a route on the provider directly, as if made elsewhere.
func (f *Fake) AddRoute(linkID, domain, host string, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, &fakeRoute{domain: domain, linkID: linkID, host: host, port: port})
}

// Calls lists the requests in order, e.g. "POST /routes play.example.com".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// SetCustomDomainsEnabled switches own domains on the provider (default on).
func (f *Fake) SetCustomDomainsEnabled(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customOff = !on
}

// SetTXTPublished makes the provider's TXT value for domain findable or not.
func (f *Fake) SetTXTPublished(domain string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published[domain] = ok
}

// SetCustomDomainState sets a claim's state directly, as the provider's own
// verifier does on a lapse ("blocked") or a pass ("verified").
func (f *Fake) SetCustomDomainState(domain, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.domain(domain); d != nil {
		d.state = state
	}
}

// SetLiveReverify makes a route put on an own domain re-prove a pending or
// blocked claim whose TXT value is published, like the provider's gate
// (default off: the put is refused until the claim is verified).
func (f *Fake) SetLiveReverify(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveReverify = on
}

// SetCustomDomainLapsesAt marks a verified claim as failing its re-checks
// (at != nil), or clears that.
func (f *Fake) SetCustomDomainLapsesAt(domain string, at *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.domain(domain); d != nil {
		d.lapsesAt = at
	}
}

// SetKitOnline marks a kit's Link as connected with the given capabilities.
func (f *Fake) SetKitOnline(linkID string, caps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.kits {
		if k.linkID == linkID {
			k.online = true
			k.caps = append([]string{}, caps...)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func handlerError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]interface{}{"success": false, "error": msg, "code": code})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, apiPrefix)
	var body map[string]interface{}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	call := r.Method + " " + path
	if r.Method == http.MethodPost && path == "/routes" {
		call += " " + str(body, "domain")
	}
	f.calls = append(f.calls, call)

	if len(f.fails) > 0 && f.fails[0].status == 0 { // a FailAfter pass
		f.fails = f.fails[1:]
	} else if len(f.fails) > 0 {
		fl := f.fails[0]
		f.fails = f.fails[1:]
		if fl.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(fl.retryAfter/time.Second)))
		}
		if fl.code == "" {
			writeJSON(w, fl.status, map[string]interface{}{"success": false, "message": "Unauthorized"})
		} else {
			handlerError(w, fl.status, fl.code, "fake failure")
		}
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"success": false, "message": "Unauthorized"})
		return
	}
	if f.loseNext {
		f.loseNext = false
		real := w
		w = httptest.NewRecorder()
		defer handlerError(real, http.StatusBadGateway, "bad_gateway", "the answer was lost")
	}

	switch {
	case r.Method == http.MethodGet && path == "/account" && f.acct != nil:
		writeJSON(w, 200, map[string]interface{}{"success": true, "account": f.acct})
	case r.Method == http.MethodGet && path == "/account":
		writeJSON(w, 200, map[string]interface{}{"success": true, "account": map[string]interface{}{
			"userId": "fake-user", "product": "managed", "managed": true, "entitlementSource": "plan",
			"suspended": false, "scopes": []string{"gateway.read", "gateway.write", "usage.read"},
			"limits": map[string]interface{}{"maxLinks": 5, "maxRoutes": nil, "maxCustomDomains": 0},
		}})
	case r.Method == http.MethodGet && path == "/check-domain":
		d := strings.ToLower(r.URL.Query().Get("domain"))
		available, reason := d != "", ""
		if d == "" {
			reason = "invalid"
		}
		writeJSON(w, 200, map[string]interface{}{"success": true, "domain": d, "available": available, "reason": reason})
	case r.Method == http.MethodGet && path == "/link-kits":
		items := []map[string]interface{}{}
		for _, k := range f.kits {
			it := map[string]interface{}{"id": k.id, "name": k.name, "link_id": k.linkID,
				"created_at": "2026-11-12T00:00:00Z", "region": k.region, "online": k.online, "caps": k.caps}
			if k.retire != nil {
				it["retire_at"] = k.retire.UTC().Format(time.RFC3339)
			}
			items = append(items, it)
		}
		live, retiring := f.kitCounts()
		writeJSON(w, 200, map[string]interface{}{"success": true, "items": items, "used": live, "retiring": retiring, "limit": f.linkLimit})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/link-kits/") && strings.HasSuffix(path, "/retire"):
		f.retireKit(w, strings.TrimSuffix(strings.TrimPrefix(path, "/link-kits/"), "/retire"))
	case r.Method == http.MethodPost && path == "/link-kits":
		if live, _ := f.kitCounts(); f.linkLimit != nil && live >= *f.linkLimit {
			handlerError(w, 403, "link_limit", "link limit reached")
			return
		}
		k := &fakeKit{id: f.nextID, name: str(body, "name"), linkID: fmt.Sprintf("link-fake%d", f.nextID), region: str(body, "region"), caps: []string{}}
		f.nextID++
		f.kits = append(f.kits, k)
		writeJSON(w, 200, map[string]interface{}{"success": true, "link_key": "lk_secret_" + k.linkID, "link_id": k.linkID, "note": "shown once"})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/link-kits/"):
		id := strings.TrimPrefix(path, "/link-kits/")
		for i, k := range f.kits {
			if k.linkID == id {
				f.kits = append(f.kits[:i], f.kits[i+1:]...)
				kept := f.routes[:0]
				for _, rt := range f.routes {
					if rt.linkID != id {
						kept = append(kept, rt)
					}
				}
				f.routes = kept
				writeJSON(w, 200, map[string]interface{}{"success": true})
				return
			}
		}
		handlerError(w, 404, "link_not_found", "link kit not found")
	case r.Method == http.MethodGet && path == "/routes":
		items := []map[string]interface{}{}
		for _, rt := range f.routes {
			items = append(items, map[string]interface{}{"domain": rt.domain, "target_ip": rt.host, "target_port": rt.port,
				"server_uuid": "", "link_id": rt.linkID, "proxy_protocol": rt.proxy})
		}
		writeJSON(w, 200, map[string]interface{}{"success": true, "items": items})
	case r.Method == http.MethodPost && path == "/routes":
		f.putRoute(w, body)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/routes/"):
		d := strings.TrimPrefix(path, "/routes/")
		for i, rt := range f.routes {
			if rt.domain == d {
				f.routes = append(f.routes[:i], f.routes[i+1:]...)
				writeJSON(w, 200, map[string]interface{}{"success": true, "message": "route deleted"})
				return
			}
		}
		handlerError(w, 404, "not_found", "route not found")
	case r.Method == http.MethodPatch && strings.HasSuffix(path, "/proxy-protocol"):
		d := strings.TrimSuffix(strings.TrimPrefix(path, "/routes/"), "/proxy-protocol")
		for _, rt := range f.routes {
			if rt.domain == d {
				en, _ := body["enabled"].(bool)
				rt.proxy = en
				writeJSON(w, 200, map[string]interface{}{"success": true, "proxyProtocol": en})
				return
			}
		}
		handlerError(w, 404, "not_found", "route not found")
	case r.Method == http.MethodGet && path == "/regions":
		writeJSON(w, 200, map[string]interface{}{"success": true,
			"hosterDomains":        []map[string]string{{"domain": "example.com", "validation": "dns"}},
			"customDomainsEnabled": !f.customOff, "cnameTarget": "route.example.com",
			"items": []map[string]interface{}{{"region": "eu", "wildcard": "*.eu.example.com", "online": true}}})
	case r.Method == http.MethodGet && path == "/custom-domains":
		items := []map[string]interface{}{}
		for _, d := range f.domains {
			items = append(items, d.view())
		}
		writeJSON(w, 200, map[string]interface{}{"success": true, "items": items})
	case r.Method == http.MethodPost && path == "/custom-domains":
		f.addDomain(w, strings.ToLower(strings.TrimSuffix(strings.TrimSpace(str(body, "domain")), ".")))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/custom-domains/") && strings.HasSuffix(path, "/txt-token"):
		d := f.domain(strings.TrimSuffix(strings.TrimPrefix(path, "/custom-domains/"), "/txt-token"))
		switch {
		case d == nil:
			handlerError(w, 404, "not_found", "No claim on that domain for your account")
		case d.state == "verified":
			handlerError(w, 400, "bad_request", "That domain is already verified")
		default:
			if d.token == "" {
				d.token = fmt.Sprintf("dylaris-verify=fake%028d", f.nextID)
				f.nextID++
			}
			writeJSON(w, 200, map[string]interface{}{"success": true, "domain": d.view()})
		}
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/custom-domains/") && strings.HasSuffix(path, "/verify-txt"):
		f.verifyDomain(w, strings.TrimSuffix(strings.TrimPrefix(path, "/custom-domains/"), "/verify-txt"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/custom-domains/"):
		name := strings.TrimPrefix(path, "/custom-domains/")
		for _, rt := range f.routes {
			if rt.domain == name {
				handlerError(w, 409, "domain_in_use", "Delete the routes on "+name+" first.")
				return
			}
		}
		for i, d := range f.domains {
			if d.domain == name {
				f.domains = append(f.domains[:i], f.domains[i+1:]...)
				writeJSON(w, 200, map[string]interface{}{"success": true})
				return
			}
		}
		handlerError(w, 404, "not_found", "No claim on that domain for your account")
	case r.Method == http.MethodGet && path == "/usage":
		writeJSON(w, 200, map[string]interface{}{"success": true,
			"usage": map[string]interface{}{"period": "2026-11-01T00:00:00Z", "edgeBytes": 1000, "relayBytes": 20},
			"final": false,
			"pool":  map[string]interface{}{"includedGb": 5000, "maxPurchaseGb": nil, "usedBytes": 1020, "percent": 0}})
	default:
		handlerError(w, 404, "not_found", "no such fake endpoint")
	}
}

func (f *Fake) putRoute(w http.ResponseWriter, body map[string]interface{}) {
	linkID, domain, host := str(body, "linkId"), str(body, "domain"), str(body, "targetHost")
	port, _ := body["targetPort"].(float64)
	if port == 0 {
		port = 25565
	}
	var kit *fakeKit
	for _, k := range f.kits {
		if k.linkID == linkID {
			kit = k
		}
	}
	if kit == nil {
		handlerError(w, 400, "link_not_found", "link kit not found")
		return
	}
	// The provider takes no route onto a retiring kit; moving one away is a
	// put naming another kit, so it is not refused here.
	if kit.retire != nil {
		handlerError(w, 409, "kit_retiring", "This link is being replaced and takes no new routes.")
		return
	}
	// The provider's target policy for API-key routes (private addresses
	// aside, which a customer Core never sends): the same shared check, so a
	// fake stricter or looser than the provider cannot hide a refused name.
	if !validate.IsServerHostname(strings.ToLower(strings.TrimSpace(host))) {
		handlerError(w, 400, "target_not_allowed", "target not allowed")
		return
	}
	if !underHoster(domain) && !f.ownDomainGate(w, domain) {
		return
	}
	for _, rt := range f.routes {
		if rt.domain == domain { // upsert: keeps proxy protocol, like the provider
			rt.linkID, rt.host, rt.port = linkID, host, int(port)
			writeJSON(w, 201, map[string]interface{}{"success": true, "message": "route updated", "domain": domain})
			return
		}
	}
	f.routes = append(f.routes, &fakeRoute{domain: domain, linkID: linkID, host: host, port: int(port)})
	writeJSON(w, 201, map[string]interface{}{"success": true, "message": "route created", "domain": domain})
}

// ownDomainGate is the provider's customDomainGate for a put on an own
// domain: a verified claim passes, a permanent block is domain_blocked, and
// anything else gets a claim (with a TXT value) and domain_unverified, whose
// message names the value like the real one, unless liveReverify proves it.
func (f *Fake) ownDomainGate(w http.ResponseWriter, domain string) bool {
	d := f.domain(domain)
	if d != nil && d.state == "permablocked" {
		handlerError(w, 403, "domain_blocked", domain+" is blocked for your account after 5 failed ownership checks.")
		return false
	}
	if d != nil && d.state == "verified" {
		return true
	}
	if d == nil {
		d = &fakeDomain{domain: domain, state: "pending"}
		f.domains = append(f.domains, d)
	}
	if d.token == "" {
		d.token = fmt.Sprintf("dylaris-verify=fake%028d", f.nextID)
		f.nextID++
	}
	if f.liveReverify && f.published[domain] {
		d.state, d.attempts, d.lapsesAt = "verified", 0, nil
		return true
	}
	handlerError(w, 403, "domain_unverified", fmt.Sprintf("Prove you own this domain first: add a TXT record _dylaris-verify.%s with the value %s, "+
		"then create the route again. DNS changes can take a few minutes to show.", domain, d.token))
	return false
}

func (f *Fake) domain(name string) *fakeDomain {
	for _, d := range f.domains {
		if d.domain == name {
			return d
		}
	}
	return nil
}

// view is the provider's customDomainView.
func (d *fakeDomain) view() map[string]interface{} {
	v := map[string]interface{}{"domain": d.domain, "state": d.state, "attempts": d.attempts}
	if d.token != "" {
		v["txtName"] = "_dylaris-verify." + d.domain
		v["txtValue"] = d.token
	}
	if d.state == "verified" && d.lapsesAt != nil {
		v["lapsesAt"] = d.lapsesAt.UTC().Format(time.RFC3339)
	}
	return v
}

// addDomain follows the provider's Add: own domains on, a domain name, the
// claim bound for a new domain, then the claim (an existing one is answered
// as it is) with a TXT value minted once.
func (f *Fake) addDomain(w http.ResponseWriter, name string) {
	if f.customOff {
		handlerError(w, 409, "custom_domains_off", "Your own domains are not enabled on this platform.")
		return
	}
	// The provider's resolveBroughtDomain: a host name of 2 to 4 labels, not
	// on or under a hoster domain.
	if !isHostname(name) {
		handlerError(w, 400, "bad_request", "invalid custom domain format")
		return
	}
	if n := len(strings.Split(name, ".")); n < 2 || n > 4 {
		handlerError(w, 400, "bad_request", "custom domain may have at most two subdomain levels")
		return
	}
	if underHoster(name) {
		handlerError(w, 400, "bad_request", "custom domain may not be a subdomain of a hoster domain ("+fakeHoster+")")
		return
	}
	d := f.domain(name)
	if d == nil {
		if len(f.domains) >= fakeClaimCap {
			handlerError(w, 409, "too_many_domains", fmt.Sprintf("You can add up to %d domains.", fakeClaimCap))
			return
		}
		d = &fakeDomain{domain: name, state: "pending"}
		f.domains = append(f.domains, d)
	}
	if d.token == "" && d.state != "verified" {
		d.token = fmt.Sprintf("dylaris-verify=fake%028d", f.nextID)
		f.nextID++
	}
	writeJSON(w, 200, map[string]interface{}{"success": true, "domain": d.view()})
}

// verifyDomain follows the provider's VerifyTXT, refusals in its order; "not
// found yet" is the plain 409 the external wrapper sends with code conflict.
func (f *Fake) verifyDomain(w http.ResponseWriter, name string) {
	d := f.domain(name)
	switch {
	case d == nil:
		handlerError(w, 404, "not_found", "No claim on that domain for your account")
		return
	case d.state == "verified" && d.lapsesAt == nil:
		handlerError(w, 400, "bad_request", "That domain is already verified")
		return
	case d.token == "":
		handlerError(w, 400, "bad_request", "Request a verification token for this domain first")
		return
	}
	if at, ok := f.checked[name]; ok && time.Since(at) < fakeVerifyCooldown {
		handlerError(w, 429, "verify_cooldown", "You checked this domain a moment ago. Try again in 5 minutes.")
		return
	}
	f.checked[name] = time.Now()
	if !f.published[name] {
		handlerError(w, 409, "conflict", "The TXT record was not found yet. DNS changes can take a few minutes to publish.")
		return
	}
	d.state, d.attempts, d.lapsesAt = "verified", 0, nil
	writeJSON(w, 200, map[string]interface{}{"success": true, "message": "Ownership verified. You can add routes on " + name + "."})
}

func (f *Fake) kitCounts() (live, retiring int) {
	for _, k := range f.kits {
		if k.retire != nil {
			retiring++
		} else {
			live++
		}
	}
	return live, retiring
}

// retireKit follows the provider's rules: nothing to free without a host
// limit, at most as many retiring kits as hosts, and a retirement budget.
func (f *Fake) retireKit(w http.ResponseWriter, linkID string) {
	var kit *fakeKit
	for _, k := range f.kits {
		if k.linkID == linkID {
			kit = k
		}
	}
	switch {
	case kit == nil:
		handlerError(w, 404, "link_not_found", "link kit not found")
		return
	case kit.retire != nil:
		writeJSON(w, 409, map[string]interface{}{"success": false, "error": "This link is already retiring.", "code": "kit_already_retiring",
			"retire_at": kit.retire.UTC().Format(time.RFC3339)})
		return
	case f.linkLimit == nil || *f.linkLimit == 0:
		handlerError(w, 409, "retire_not_needed", "This plan has no host limit, so nothing needs to be replaced.")
		return
	}
	budget := 4 * *f.linkLimit
	if f.retireBudget != nil {
		budget = *f.retireBudget
	}
	if _, retiring := f.kitCounts(); retiring >= *f.linkLimit {
		handlerError(w, 409, "retire_limit", "All replacement slots are in use.")
		return
	}
	if f.retired >= budget {
		handlerError(w, 409, "retire_budget", "The monthly replacements are used up.")
		return
	}
	at := time.Now().Add(8 * time.Hour).UTC().Truncate(time.Second)
	kit.retire = &at
	f.retired++
	writeJSON(w, 200, map[string]interface{}{"success": true, "link_id": linkID, "retire_at": at.Format(time.RFC3339)})
}

func str(m map[string]interface{}, k string) string {
	s, _ := m[k].(string)
	return s
}
