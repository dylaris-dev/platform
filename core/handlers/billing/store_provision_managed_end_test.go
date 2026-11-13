package billing

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
)

// capsStore keeps the four storages a provision writes as STATE, so a sequence
// of store calls can be judged by what the account holds at the end. The admin
// layers (admin_max_links, admin:<id> scopes, per-region user traffic rows) sit
// in the same maps under their own keys, which is how they share storage in
// the real tables.
type capsStore struct {
	*storeLinkFakeStore
	maxNodes, maxLinks *int64
	routes             map[string]*int
	domains            map[string]*int64
	traffic            map[string]models.TrafficLimit // scope/region/kind
	// capSince is user_billing.custom_domain_cap_since: the managed plan ended.
	capSince bool
}

func newCapsStore() *capsStore {
	return &capsStore{
		storeLinkFakeStore: &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}},
		routes:             map[string]*int{},
		domains:            map[string]*int64{},
		traffic:            map[string]models.TrafficLimit{},
	}
}

func (c *capsStore) SetUserPurchasedEntitlement(_ string, n *int64, setN bool, l *int64, setL bool) error {
	if setN {
		c.maxNodes = n
	}
	if setL {
		c.maxLinks = l
	}
	return nil
}
func (c *capsStore) SetGatewayRouteLimit(scope string, max *int) error {
	c.routes[scope] = max
	return nil
}
func (c *capsStore) DeleteGatewayRouteLimit(scope string) error { delete(c.routes, scope); return nil }
func (c *capsStore) SetGatewayCustomDomainLimit(scope string, max *int64) error {
	c.domains[scope] = max
	return nil
}
func (c *capsStore) DeleteGatewayCustomDomainLimit(scope string) error {
	delete(c.domains, scope)
	return nil
}

// Same semantics as the Postgres store: setting clears the marker, a clear
// that removed a row sets it.
func (c *capsStore) SetUserCustomDomainLimit(userID string, max *int64) error {
	c.domains["user:"+userID] = max
	c.capSince = false
	return nil
}
func (c *capsStore) ClearUserCustomDomainLimit(userID string) error {
	if _, ok := c.domains["user:"+userID]; ok {
		delete(c.domains, "user:"+userID)
		c.capSince = true
	}
	return nil
}

// Same semantics as the Postgres store: only the ("*", edge) row is replaced.
func (c *capsStore) ReplaceTrafficLimits(scope string, rows []models.TrafficLimit) error {
	delete(c.traffic, scope+"/*/edge")
	for _, r := range rows {
		c.traffic[scope+"/"+r.Region+"/"+r.Kind] = r
	}
	return nil
}
func (c *capsStore) DeleteTrafficLimit(scope, region, kind string) error {
	delete(c.traffic, scope+"/"+region+"/"+kind)
	return nil
}

func ptrStr[T int | int64](p *T) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

// snapshot is the account's whole entitlement as one comparable string.
func (c *capsStore) snapshot() string {
	var parts []string
	parts = append(parts, "nodes="+ptrStr(c.maxNodes), "links="+ptrStr(c.maxLinks))
	for k, v := range c.routes {
		parts = append(parts, "route "+k+"="+ptrStr(v))
	}
	for k, v := range c.domains {
		parts = append(parts, "domains "+k+"="+ptrStr(v))
	}
	for k, v := range c.traffic {
		parts = append(parts, "traffic "+k+"="+ptrStr(v.IncludedGB)+"/"+ptrStr(v.MaxPurchaseGB))
	}
	if c.capSince {
		parts = append(parts, "custom_domain_cap_since")
	}
	return state(parts...)
}

func state(parts ...string) string {
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func (c *capsStore) provision(t *testing.T, action string, body map[string]interface{}) {
	t.Helper()
	h := &StoreHandler{d: Deps{Store: c, Redis: newStoreLinkRedis(t), StoreEnabled: true, StoreSharedKey: storeLinkTestKey}}
	h.d.Billing = services.NewBillingLifecycleService(c, services.NewQueueService(h.d.Redis), nil, "https://panel.example.com", 48*time.Hour, true)
	b := map[string]interface{}{"uuid": "u1", "action": action}
	for k, v := range body {
		b[k] = v
	}
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", b, storeLinkTestKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", action, rec.Code, rec.Body.String())
	}
}

// The payloads below are what the store sends (website dylaris-api
// managedEntitlement / purchasedEntitlement), field for field.
func managedBody() map[string]interface{} {
	return map[string]interface{}{
		"maxNodes": 0, "maxLinks": 1, "maxRoutes": 50, "maxCustomDomains": "unlimited",
		"trafficLimits": []map[string]interface{}{{"region": "*", "kind": "edge", "includedGb": 5000, "maxPurchaseGb": 5000}},
	}
}

func routeOnlyBody() map[string]interface{} {
	return map[string]interface{}{"maxNodes": 0, "maxLinks": 1, "maxRoutes": 3}
}

func byonBody() map[string]interface{} {
	return map[string]interface{}{"maxNodes": 2, "maxLinks": 0, "maxRoutes": 6}
}

// A managed plan that ended on "suspend" (unlink, or cancelled with nothing
// beside it) must not leave its caps for the next, cheaper activate: that is a
// second store account buying route-only and keeping 5 TB, unlimited custom
// domains and 50 addresses.
func TestProvisionManagedCapsDoNotOutliveThePlan(t *testing.T) {
	managed := []string{"nodes=nil", "links=1", "domains user:u1=nil", "route user:u1=50", "traffic user:u1/*/edge=5000/5000"}
	routeOnly := []string{"nodes=nil", "links=1", "route user:u1=3"}
	byon := []string{"nodes=2", "links=nil", "route user:u1=6"}
	// Operator-set layers that live in the same tables, under other keys.
	admin := []string{"domains admin:u1=7", "route admin:u1=9", "traffic user:u1/eu-central/edge=100/0", "traffic user:u1/*/relay=10/nil"}
	with := func(a, b []string) string { return state(append(append([]string{}, a...), b...)...) }
	managedState, routeOnlyState, byonState := state(managed...), state(routeOnly...), state(byon...)
	// The marker the over-limit sweep reads: set when an activate removed the
	// managed custom-domain row, never for an account that never held one.
	ended := []string{"custom_domain_cap_since"}
	endedRouteOnly := append(append([]string{}, routeOnly...), ended...)
	type step struct {
		action string
		body   func() map[string]interface{}
	}
	act := func(b func() map[string]interface{}) step { return step{"activate", b} }
	suspend := step{"suspend", nil}
	pastDue := step{"past_due", nil}
	// The store's wipe for an ended managed plan (WipeManaged): [] + null.
	wipeBody := func() map[string]interface{} {
		b := routeOnlyBody()
		b["trafficLimits"], b["maxCustomDomains"] = []interface{}{}, nil
		return b
	}
	cases := []struct {
		name      string
		withAdmin bool
		// adminFirst sets the operator layers before the first step instead of
		// after it.
		adminFirst bool
		steps      []step
		wantFinal  string
		wantBefore string // state before the last step, "" = not checked
	}{
		{name: "suspend alone keeps the managed caps (a traffic cutoff is not the end of the plan)",
			steps: []step{act(managedBody), suspend}, wantFinal: managedState},
		{name: "managed, suspend, route-only from another store account: route-only caps only",
			steps: []step{act(managedBody), suspend, act(routeOnlyBody)}, wantFinal: state(endedRouteOnly...)},
		{name: "managed, suspend, BYON: BYON caps only",
			steps: []step{act(managedBody), suspend, act(byonBody)}, wantFinal: with(byon, ended)},
		{name: "managed, suspend, managed again: everything restored",
			steps: []step{act(managedBody), suspend, act(managedBody)}, wantFinal: managedState},
		{name: "admin layers survive the end of the plan",
			withAdmin: true, steps: []step{act(managedBody), suspend, act(routeOnlyBody)}, wantFinal: with(endedRouteOnly, admin)},
		{name: "admin layers set after a managed activate survive suspend",
			withAdmin: true, steps: []step{act(managedBody), suspend}, wantFinal: with(managed, admin)},
		{name: "past_due changes no entitlement",
			steps: []step{act(managedBody), pastDue}, wantFinal: managedState},
		{name: "route-only never managed: suspend and activate as before",
			steps: []step{act(routeOnlyBody), suspend, act(routeOnlyBody)}, wantBefore: routeOnlyState, wantFinal: routeOnlyState},
		{name: "BYON never managed: suspend and activate as before",
			steps: []step{act(byonBody), suspend, act(byonBody)}, wantBefore: byonState, wantFinal: byonState},
		{name: "operator rows from before the plan survive the managed activate; the * row is the store's",
			withAdmin: true, adminFirst: true, steps: []step{act(managedBody)}, wantFinal: with(managed, admin)},
		{name: "repeated managed syncs are idempotent and keep the operator rows",
			withAdmin: true, adminFirst: true, steps: []step{act(managedBody), act(managedBody), act(managedBody)},
			wantBefore: with(managed, admin), wantFinal: with(managed, admin)},
		{name: "repeated managed syncs without operator rows: as before",
			steps: []step{act(managedBody), act(managedBody)}, wantBefore: managedState, wantFinal: managedState},
		{name: "the wipe (trafficLimits []) removes only the * row",
			withAdmin: true, adminFirst: true, steps: []step{act(managedBody), act(wipeBody)}, wantFinal: with(endedRouteOnly, admin)},
		{name: "the wipe without operator rows: route-only caps only",
			steps: []step{act(managedBody), act(wipeBody)}, wantFinal: state(endedRouteOnly...)},
		{name: "managed again after it ended clears the marker",
			steps: []step{act(managedBody), act(wipeBody), act(managedBody)}, wantBefore: state(endedRouteOnly...), wantFinal: managedState},
		{name: "the wipe sent twice keeps one marker, a later route-only activate too",
			steps: []step{act(managedBody), act(wipeBody), act(wipeBody), act(routeOnlyBody)}, wantFinal: state(endedRouteOnly...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapsStore()
			for i, s := range tc.steps {
				// Set by an operator while the plan runs, or before it.
				if tc.withAdmin && ((tc.adminFirst && i == 0) || (!tc.adminFirst && i == 1)) {
					seven, nine := int64(7), 9
					c.domains["admin:u1"] = &seven
					c.routes["admin:u1"] = &nine
					inc, buy := int64(100), int64(0)
					c.traffic["user:u1/eu-central/edge"] = models.TrafficLimit{Region: "eu-central", Kind: "edge", IncludedGB: &inc, MaxPurchaseGB: &buy}
					relay := int64(10)
					c.traffic["user:u1/*/relay"] = models.TrafficLimit{Region: "*", Kind: "relay", IncludedGB: &relay}
				}
				if i == len(tc.steps)-1 && tc.wantBefore != "" {
					if got := c.snapshot(); got != tc.wantBefore {
						t.Fatalf("before the last step:\n got  %s\n want %s", got, tc.wantBefore)
					}
				}
				var body map[string]interface{}
				if s.body != nil {
					body = s.body()
				}
				c.provision(t, s.action, body)
			}
			if got := c.snapshot(); got != tc.wantFinal {
				t.Fatalf("final state:\n got  %s\n want %s", got, tc.wantFinal)
			}
		})
	}
}
