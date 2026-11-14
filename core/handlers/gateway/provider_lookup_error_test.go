package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"
)

// faultyManagedStore fails the route table read on demand, like the provider's
// database during a pgbouncer flap.
type faultyManagedStore struct {
	*managedStore
	coreErr error
}

func (f *faultyManagedStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	if f.coreErr != nil {
		return nil, f.coreErr
	}
	return f.managedStore.ListCoreLinkRoutes()
}

func newFaultyEnv(t *testing.T, managed bool) (*managedEnv, *faultyManagedStore) {
	t.Helper()
	m := newManagedEnv(t, managed)
	fs := &faultyManagedStore{managedStore: m.ms}
	m.e.h.d.Store = fs
	m.ms.links[linkDomain] = store.LinkRouteEdition{Domain: linkDomain, OwnerID: "owner-1", Java: true, Bedrock: true, Key: make([]byte, 16)}
	return m, fs
}

// lookupCalls runs one call on the panel path and its twin on the external
// API: status and body of each.
type lookupCall struct {
	name  string
	panel func(m *managedEnv, domain string) *httptest.ResponseRecorder
	ext   func(m *managedEnv, domain string) *httptest.ResponseRecorder
}

func lookupCalls() []lookupCall {
	extDo := func(m *managedEnv, h http.HandlerFunc, method, domain string, body interface{}) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, extReq(method, "/", "owner-1", false, map[string]string{"domain": domain}, body))
		return rec
	}
	return []lookupCall{
		{"voice on",
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, d, `{"enabled":true}`, "owner-1", false)
			},
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return extDo(m, m.x.SetRouteVoice, http.MethodPut, d, map[string]interface{}{"enabled": true})
			}},
		{"Bedrock on",
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return linkCall(m.e, m.e.h.SetLinkRouteBedrock, http.MethodPut, d, `{"java":true,"bedrock":true}`, "owner-1", false)
			},
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return extDo(m, m.x.SetRouteBedrock, http.MethodPut, d, map[string]interface{}{"java": true, "bedrock": true})
			}},
		{"Floodgate key",
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return linkCall(m.e, m.e.h.GetLinkRouteFloodgateKey, http.MethodGet, d, "", "owner-1", false)
			},
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return extDo(m, m.x.GetRouteFloodgateKey, http.MethodGet, d, nil)
			}},
	}
}

func extCodeOf(rec *httptest.ResponseRecorder) string {
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	c, _ := out["code"].(string)
	return c
}

// A route table that cannot be read is a server error on both paths, never
// "Route not found": a customer Core must not read a database fault as the
// address being gone. An error never allows anything either.
func TestProviderRouteLookupErrorIsAServerError(t *testing.T) {
	for _, c := range lookupCalls() {
		// inRedis: the live route still shows the caller as its owner, so the
		// ownership check passes and the stored row read is what fails (the
		// shape of a database flap on the provider).
		for _, inRedis := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, route in Redis %v", c.name, inRedis), func(t *testing.T) {
				m, fs := newFaultyEnv(t, true)
				if inRedis {
					seedLinkRoute(t, m.e.h.d.Redis, linkDomain, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
				}
				if inRedis && c.name == "Floodgate key" {
					// Ownership is proven by the live route and the key row
					// is read fine: serving it is right, no refusal involved.
					fs.coreErr = errors.New("pq: connection reset by peer")
					if rec := c.ext(m, linkDomain); rec.Code != http.StatusOK {
						t.Fatalf("external: %d %s, want the key", rec.Code, rec.Body.String())
					}
					return
				}
				lookupErrorCase(t, c, m, fs)
			})
		}
	}
}

func lookupErrorCase(t *testing.T, c lookupCall, m *managedEnv, fs *faultyManagedStore) {
	t.Helper()
	fs.coreErr = errors.New("pq: connection reset by peer")
	if rec := c.panel(m, linkDomain); rec.Code != http.StatusInternalServerError {
		t.Fatalf("panel: %d %s, want 500", rec.Code, rec.Body.String())
	}
	if rec := c.ext(m, linkDomain); rec.Code != http.StatusInternalServerError || extCodeOf(rec) != "internal" {
		t.Fatalf("external: %d %s, want 500 internal", rec.Code, rec.Body.String())
	}
	if _, on := m.ms.voice[linkDomain]; on {
		t.Fatal("a lookup error turned voice on")
	}
}

// A voice permission that cannot be read is a server error, not
// voice_not_allowed; it still allows nothing.
func TestProviderVoicePermissionReadErrorIsAServerError(t *testing.T) {
	for _, managed := range []bool{false, true} {
		m, _ := newFaultyEnv(t, managed)
		m.ms.permErr = errors.New("pq: connection reset by peer")
		c := lookupCalls()[0]
		if rec := c.panel(m, linkDomain); rec.Code != http.StatusInternalServerError {
			t.Fatalf("managed=%v panel: %d %s, want 500", managed, rec.Code, rec.Body.String())
		}
		if rec := c.ext(m, linkDomain); rec.Code != http.StatusInternalServerError || extCodeOf(rec) != "internal" {
			t.Fatalf("managed=%v external: %d %s, want 500 internal", managed, rec.Code, rec.Body.String())
		}
		if _, on := m.ms.voice[linkDomain]; on {
			t.Fatal("a read error turned voice on")
		}
	}
}

// A managed plan lookup that fails is a server error where the global rules
// would refuse; where they allow, the answer is unchanged (nothing is granted
// on the lookup's behalf, nothing refused because of it).
func TestProviderManagedLookupErrorIsAServerError(t *testing.T) {
	m, _ := newFaultyEnv(t, true)
	delete(m.ms.links, linkDomain) // adding Bedrock, the path the plan decides
	m.ms.limitErr = errors.New("pq: connection reset by peer")
	for _, c := range lookupCalls()[:2] {
		if rec := c.ext(m, linkDomain); rec.Code != http.StatusInternalServerError || extCodeOf(rec) != "internal" {
			t.Fatalf("%s, global off: %d %s, want 500 internal", c.name, rec.Code, rec.Body.String())
		}
	}
	if len(m.ms.voice) != 0 {
		t.Fatal("a lookup error turned voice on")
	}
	m.ms.global, m.ms.audience = "true", "all"
	for _, c := range lookupCalls()[:2] {
		if rec := c.ext(m, linkDomain); rec.Code != http.StatusOK {
			t.Fatalf("%s, global on: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
}

// The Bedrock setting that cannot be read is a server error on both paths
// (a route-only address, adding Bedrock), never bedrock_disabled, which the
// customer's sweep treats as final; it still allows nothing.
func TestProviderBedrockSettingReadErrorIsAServerError(t *testing.T) {
	m, _ := newFaultyEnv(t, false)
	delete(m.ms.links, linkDomain)
	m.ms.settingErr = errors.New("pq: connection reset by peer")
	c := lookupCalls()[1]
	if rec := c.panel(m, linkDomain); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "bedrock_disabled") {
		t.Fatalf("panel: %d %s, want 500", rec.Code, rec.Body.String())
	}
	if rec := c.ext(m, linkDomain); rec.Code != http.StatusInternalServerError || extCodeOf(rec) != "internal" {
		t.Fatalf("external: %d %s, want 500 internal", rec.Code, rec.Body.String())
	}
	if len(m.ms.links) != 0 {
		t.Fatal("a read error gave the address Bedrock")
	}
}

// The answers when the lookup SUCCEEDS stay byte for byte what they were
// (bodies captured from the handlers before this change), except that an
// external refusal of an account without a managed plan now says a plan
// would lift it (E2E D3, deliberate).
func TestProviderRefusalsUnchanged(t *testing.T) {
	const (
		panelNotFound  = `{"message":"Route not found","success":false}` + "\n"
		extNotFound    = `{"code":"not_found","error":"Route not found","success":false}` + "\n"
		panelVoiceNo   = `{"code":"voice_not_allowed","message":"Voice chat is not available for this account.","success":false}` + "\n"
		extVoiceNo     = `{"code":"voice_not_allowed","error":"Voice chat is not available for this account. It needs an active managed gateway plan.","success":false}` + "\n"
		panelNoKey     = `{"message":"This address has no Floodgate key yet. Give it Bedrock first.","success":false}` + "\n"
		extNoKey       = `{"code":"not_found","error":"This address has no Floodgate key yet. Give it Bedrock first.","success":false}` + "\n"
		panelBedrockNo = `{"code":"bedrock_disabled","message":"Bedrock is turned off on this platform.","success":false}` + "\n"
		extBedrockNo   = `{"code":"bedrock_disabled","error":"Bedrock is turned off on this platform. It needs an active managed gateway plan.","success":false}` + "\n"
	)
	check := func(t *testing.T, what string, rec *httptest.ResponseRecorder, status int, body string) {
		t.Helper()
		if rec.Code != status || rec.Body.String() != body {
			t.Fatalf("%s: %d %q, want %d %q", what, rec.Code, rec.Body.String(), status, body)
		}
	}
	calls := lookupCalls()
	for _, c := range calls {
		m, _ := newFaultyEnv(t, false)
		check(t, c.name+" panel, no such route", c.panel(m, "nope.eu.dylaris.com"), http.StatusNotFound, panelNotFound)
		check(t, c.name+" external, no such route", c.ext(m, "nope.eu.dylaris.com"), http.StatusNotFound, extNotFound)
	}
	m, _ := newFaultyEnv(t, false)
	check(t, "voice panel, not allowed", calls[0].panel(m, linkDomain), http.StatusConflict, panelVoiceNo)
	check(t, "voice external, not allowed", calls[0].ext(m, linkDomain), http.StatusConflict, extVoiceNo)
	delete(m.ms.links, linkDomain)
	check(t, "Bedrock panel, not allowed", calls[1].panel(m, linkDomain), http.StatusConflict, panelBedrockNo)
	check(t, "Bedrock external, not allowed", calls[1].ext(m, linkDomain), http.StatusConflict, extBedrockNo)
	check(t, "key panel, none yet", calls[2].panel(m, linkDomain), http.StatusNotFound, panelNoKey)
	check(t, "key external, none yet", calls[2].ext(m, linkDomain), http.StatusNotFound, extNoKey)
}

// DeleteLinkRoute and SetLinkRouteProxyProtocol: a route table that cannot be
// read is 500 (internal), never "Route not found". The customer's outbox takes
// not_found on a delete as done and would forget a route the provider keeps.
func TestProviderDeleteAndProxyLookupErrorIsAServerError(t *testing.T) {
	ext := func(m *managedEnv, h http.HandlerFunc, method, domain string, body interface{}) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, extReq(method, "/", "owner-1", false, map[string]string{"domain": domain}, body))
		return rec
	}
	calls := []lookupCall{
		{"delete",
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return linkCall(m.e, m.e.h.DeleteLinkRoute, http.MethodDelete, d, "", "owner-1", false)
			},
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return ext(m, m.x.DeleteRoute, http.MethodDelete, d, nil)
			}},
		{"proxy protocol",
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return linkCall(m.e, m.e.h.SetLinkRouteProxyProtocol, http.MethodPatch, d, `{"enabled":false}`, "owner-1", false)
			},
			func(m *managedEnv, d string) *httptest.ResponseRecorder {
				return ext(m, m.x.SetRouteProxyProtocol, http.MethodPatch, d, map[string]bool{"enabled": false})
			}},
	}
	const (
		panelDeleteNotFound = "Route not found\n"
		panelProxyNotFound  = `{"message":"Route not found","success":false}` + "\n"
		extNotFound         = `{"code":"not_found","error":"Route not found","success":false}` + "\n"
		panelDeleted        = `{"message":"Route deleted","success":true}` + "\n"
	)
	for i, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			m, fs := newFaultyEnv(t, true)
			m.x.requireGateway = func(next http.HandlerFunc) http.HandlerFunc { return next }
			fs.coreErr = errors.New("pq: connection reset by peer")
			if rec := c.panel(m, linkDomain); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "not found") {
				t.Fatalf("panel: %d %q, want 500", rec.Code, rec.Body.String())
			}
			if rec := c.ext(m, linkDomain); rec.Code != http.StatusInternalServerError || extCodeOf(rec) != "internal" {
				t.Fatalf("external: %d %s, want 500 internal", rec.Code, rec.Body.String())
			}
			// Answers to a lookup that succeeds are unchanged.
			fs.coreErr = nil
			panelNotFound := panelProxyNotFound
			if i == 0 {
				panelNotFound = panelDeleteNotFound
			}
			if rec := c.panel(m, "nope.eu.dylaris.com"); rec.Code != http.StatusNotFound || rec.Body.String() != panelNotFound {
				t.Fatalf("panel, no such route: %d %q", rec.Code, rec.Body.String())
			}
			if rec := c.ext(m, "nope.eu.dylaris.com"); rec.Code != http.StatusNotFound || rec.Body.String() != extNotFound {
				t.Fatalf("external, no such route: %d %q", rec.Code, rec.Body.String())
			}
		})
	}
}
