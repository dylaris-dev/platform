package remotegw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is a NON-test file on purpose: other packages' tests import the
// fake. Nothing on a production path may call it.

// ContractKeys are the JSON keys the client depends on, per response object.
// The fake is checked against them in contract_test.go and the real provider
// handlers in handlers/gateway/external_contract_test.go, so neither can drift
// from the other without a red test. Add keys, never rename.
var ContractKeys = map[string][]string{
	"account":        {"userId", "product", "managed", "entitlementSource", "suspended", "scopes", "limits"},
	"account.limits": {"maxLinks", "maxRoutes", "maxCustomDomains"},
	"check-domain":   {"success", "domain", "available", "reason"},
	"link-kits":      {"success", "items"},
	"link-kits.item": {"id", "name", "link_id", "created_at", "region", "caps"},
	"link-kits.mint": {"success", "link_key", "link_id"},
	"routes":         {"success", "items"},
	"routes.item":    {"domain", "target_ip", "target_port", "link_id", "proxy_protocol"},
	"regions":        {"success", "hosterDomains", "customDomainsEnabled", "cnameTarget", "items"},
	"regions.item":   {"region", "wildcard", "online"},
	"usage":          {"success", "usage", "final"},
	"usage.usage":    {"period", "edgeBytes", "relayBytes"},
}

type fakeKit struct {
	id     int
	name   string
	linkID string
	region string
	online bool
	caps   []string
}

type fakeRoute struct {
	domain, linkID, host string
	port                 int
	proxy                bool
}

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
	acct   *Account // nil: the default managed account
}

func NewFake(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{key: "dyl_fake_provider_key", nextID: 1}
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
func (f *Fake) SetAccount(a Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acct = &a
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
			items = append(items, map[string]interface{}{"id": k.id, "name": k.name, "link_id": k.linkID,
				"created_at": "2026-11-12T00:00:00Z", "region": k.region, "online": k.online, "caps": k.caps})
		}
		writeJSON(w, 200, map[string]interface{}{"success": true, "items": items, "used": len(items), "limit": nil})
	case r.Method == http.MethodPost && path == "/link-kits":
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
			"customDomainsEnabled": true, "cnameTarget": "route.example.com",
			"items": []map[string]interface{}{{"region": "eu", "wildcard": "*.eu.example.com", "online": true}}})
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
	known := false
	for _, k := range f.kits {
		known = known || k.linkID == linkID
	}
	if !known {
		handlerError(w, 400, "link_not_found", "link kit not found")
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

func str(m map[string]interface{}, k string) string {
	s, _ := m[k].(string)
	return s
}
