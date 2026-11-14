package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// The capsStore below keeps the billing status as state too, so "suspend on an
// account that is already suspended" really takes Suspend's no-op branch, and a
// manual status change afterwards is judged by what the account then holds.

func (c *capsStore) SetUserBillingStatus(userID, status string, graceUntil, suspendedAt *time.Time) error {
	c.statusWrites++
	c.billingStatus = status
	return c.storeLinkFakeStore.SetUserBillingStatus(userID, status, graceUntil, suspendedAt)
}

// Same rule as storeLinkFakeStore's, but writing through the stateful setter.
func (c *capsStore) SetUserBillingStatusIf(userID, status string, graceUntil, suspendedAt *time.Time, from []string) (bool, error) {
	if from != nil {
		ok := false
		for _, s := range from {
			ok = ok || s == c.billingStatus || (c.billingStatus == "" && s == "active")
		}
		if !ok {
			return false, nil
		}
	}
	return true, c.SetUserBillingStatus(userID, status, graceUntil, suspendedAt)
}

// The managed marker, read by HasManagedProduct, ManagedPoolVerdictFor and
// /store/usage, comes out of the same map the provision writes.
func (c *capsStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if l, ok := c.traffic[scope+"/"+region+"/"+kind]; ok {
		return &l, nil
	}
	return nil, nil
}
func (c *capsStore) GetTrafficUsage(userID string, p time.Time) (*store.TrafficUsage, error) {
	return &store.TrafficUsage{UserID: userID, Period: p, EdgeBytes: 7}, nil
}
func (c *capsStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return nil, nil
}
func (c *capsStore) BackupBytesByOwnerOnOwnStorage(string) (int64, error) { return 0, nil }
func (c *capsStore) ListBeamIssuerKeysByOwner(string) ([]models.BeamIssuerKey, error) {
	return c.beamKeys, nil
}
func (c *capsStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) { return nil, nil }

// A delete that fails must answer 500 so the store keeps the push pending.
func (c *capsStore) failingDeletes() {
	c.deleteErr = errors.New("db down")
}

// provisionStep is one store call of a sequence.
type provisionStep struct {
	action string
	body   func() map[string]interface{}
}

func suspendEnded() provisionStep {
	return provisionStep{"suspend", func() map[string]interface{} { return map[string]interface{}{"entitlementEnded": true} }}
}

// The store marks the end of the entitlement (plan or grant ended as the last
// row, unlink, account deletion) with entitlementEnded on suspend. Only then
// do the two managed-only rows go, with the same deletes an activate without
// managed runs. A bare suspend is a traffic cutoff of a tenant who still pays,
// and the store bills that pool from the "*" row: it must keep both.
func TestProvisionSuspendEntitlementEnded(t *testing.T) {
	managed := []string{"nodes=nil", "links=1", "domains user:u1=nil", "route user:u1=50", "traffic user:u1/*/edge=5000/5000"}
	// After the end: max_links and the 50-address row stay (owner decision Q1),
	// the "*" row and the domain row are gone, and the marker is set.
	endedManaged := []string{"nodes=nil", "links=1", "route user:u1=50", "custom_domain_cap_since"}
	routeOnly := []string{"nodes=nil", "links=1", "route user:u1=3"}
	admin := []string{"domains admin:u1=7", "route admin:u1=9", "traffic user:u1/eu-central/edge=100/0", "traffic user:u1/*/relay=10/nil"}
	with := func(a, b []string) string { return state(append(append([]string{}, a...), b...)...) }
	act := func(b func() map[string]interface{}) provisionStep { return provisionStep{"activate", b} }
	bare := provisionStep{"suspend", nil}
	flagFalse := provisionStep{"suspend", func() map[string]interface{} { return map[string]interface{}{"entitlementEnded": false} }}
	cases := []struct {
		name      string
		withAdmin bool
		steps     []provisionStep
		want      string
	}{
		{name: "bare suspend (traffic cutoff) keeps both rows",
			steps: []provisionStep{act(managedBody), bare}, want: state(managed...)},
		{name: "entitlementEnded false is a bare suspend",
			steps: []provisionStep{act(managedBody), flagFalse}, want: state(managed...)},
		{name: "plan end removes the * row and the domain row, keeps links and routes",
			steps: []provisionStep{act(managedBody), suspendEnded()}, want: state(endedManaged...)},
		{name: "plan end after a cutoff (already suspended) still removes them",
			steps: []provisionStep{act(managedBody), bare, suspendEnded()}, want: state(endedManaged...)},
		{name: "plan end resent is harmless and keeps one marker",
			steps: []provisionStep{act(managedBody), suspendEnded(), suspendEnded()}, want: state(endedManaged...)},
		{name: "plan end leaves the operator layers alone",
			withAdmin: true, steps: []provisionStep{act(managedBody), suspendEnded()}, want: with(endedManaged, admin)},
		{name: "never managed: plan end changes no entitlement",
			steps: []provisionStep{act(routeOnlyBody), suspendEnded()}, want: state(routeOnly...)},
		{name: "managed again after the end restores everything",
			steps: []provisionStep{act(managedBody), suspendEnded(), act(managedBody)}, want: state(managed...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapsStore()
			for i, s := range tc.steps {
				if tc.withAdmin && i == 1 {
					seven, nine := int64(7), 9
					c.domains["admin:u1"] = &seven
					c.routes["admin:u1"] = &nine
					inc, buy := int64(100), int64(0)
					c.traffic["user:u1/eu-central/edge"] = models.TrafficLimit{Region: "eu-central", Kind: "edge", IncludedGB: &inc, MaxPurchaseGB: &buy}
					relay := int64(10)
					c.traffic["user:u1/*/relay"] = models.TrafficLimit{Region: "*", Kind: "relay", IncludedGB: &relay}
				}
				var body map[string]interface{}
				if s.body != nil {
					body = s.body()
				}
				c.provision(t, s.action, body)
			}
			if got := c.snapshot(); got != tc.want {
				t.Fatalf("final state:\n got  %s\n want %s", got, tc.want)
			}
			if c.billingStatus != "suspended" && tc.steps[len(tc.steps)-1].action == "suspend" {
				t.Fatalf("billing status %q after a suspend", c.billingStatus)
			}
		})
	}
}

// The cutoff-then-cancel order: the second suspend is Suspend's no-op (no
// status write, so the hard-cutoff clock is not pushed back), and the deletes
// run anyway.
func TestProvisionSuspendEntitlementEndedOnSuspendedAccount(t *testing.T) {
	c := newCapsStore()
	c.provision(t, "activate", managedBody())
	c.provision(t, "suspend", nil)
	writes := c.statusWrites
	c.provision(t, "suspend", map[string]interface{}{"entitlementEnded": true})
	if c.statusWrites != writes {
		t.Fatalf("the second suspend wrote the status again (%d -> %d writes)", writes, c.statusWrites)
	}
	if managed, _ := services.HasManagedProduct(c, "u1"); managed {
		t.Fatal("the managed marker survived the plan end on a suspended account")
	}
}

// A failed delete answers 500: the store keeps core_sync_pending and resends.
func TestProvisionSuspendEntitlementEndedDeleteFails(t *testing.T) {
	c := newCapsStore()
	c.provision(t, "activate", managedBody())
	c.failingDeletes()
	h := &StoreHandler{d: Deps{Store: c, Redis: newStoreLinkRedis(t), StoreEnabled: true, StoreSharedKey: storeLinkTestKey}}
	h.d.Billing = services.NewBillingLifecycleService(c, services.NewQueueService(h.d.Redis), nil, "https://panel.example.com", 48*time.Hour, true)
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "suspend", "entitlementEnded": true}, storeLinkTestKey))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

// Compatibility in both directions. An OLD store sends no entitlementEnded at
// all (covered above: bare suspend). A NEW store may meet an older Core, which
// decodes without DisallowUnknownFields; this pins that Core keeps ignoring
// fields it does not know, on every action, so the next additive field is safe
// in the same way.
func TestProvisionIgnoresUnknownFields(t *testing.T) {
	for _, action := range []string{"activate", "past_due", "suspend"} {
		t.Run(action, func(t *testing.T) {
			c := newCapsStore()
			body := map[string]interface{}{"someFutureField": map[string]interface{}{"x": 1}}
			if action == "activate" {
				body = managedBody()
				body["someFutureField"] = true
			}
			c.provision(t, action, body) // fails the test on anything but 200
		})
	}
}

// managedView is what every consumer of the managed marker sees: the voice
// and Bedrock exemptions and the external API (HasManagedProduct), the pool
// verdict and grace spell (ManagedPoolVerdictFor), the store's billing read
// (/store/usage managedPool) and the beam issuer records.
type managedView struct {
	marker, pool, usagePool, beamRecord bool
}

const testBeamKid = "0123456789abcdef0123456789abcdef"

func (c *capsStore) view(t *testing.T, h *StoreHandler) managedView {
	t.Helper()
	var v managedView
	var err error
	if v.marker, err = services.HasManagedProduct(c, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, v.pool, err = services.ManagedPoolVerdictFor(c, "u1", time.Now()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/store/usage?uuid=u1", nil)
	req.Header.Set("X-Store-Key", storeLinkTestKey)
	rec := httptest.NewRecorder()
	h.GetUsage(rec, req)
	var out map[string]json.RawMessage
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body.String())
	}
	v.usagePool = string(out["managedPool"]) != "null"
	if err := services.PublishBeamIssuers(context.Background(), c, h.d.Redis, h.d.SuspendGrace, "u1"); err != nil {
		t.Fatal(err)
	}
	n, err := h.d.Redis.Exists(context.Background(), services.BeamIssuerKeyPrefix+testBeamKid).Result()
	if err != nil {
		t.Fatal(err)
	}
	v.beamRecord = n == 1
	return v
}

// The bug this change closes, end to end at handler level, and its counter-case.
//
// Plan end: activate managed -> suspend + entitlementEnded -> an operator sets
// the account active or past_due by hand (status only, no entitlement). The
// account must then hold no managed product anywhere.
//
// Traffic cutoff: activate managed -> bare suspend. Everything the tenant pays
// for is still there DURING the cutoff (the store bills the pool from it), and
// when the guard lifts the status alone brings it all back; the store's
// activate after the release then changes nothing.
func TestManagedMarkerAcrossSuspendAndManualStatus(t *testing.T) {
	all := managedView{marker: true, pool: true, usagePool: true, beamRecord: true}
	none := managedView{}
	type op struct {
		name string
		run  func(t *testing.T, c *capsStore, h *StoreHandler)
	}
	provision := func(action string, body func() map[string]interface{}) op {
		return op{action, func(t *testing.T, c *capsStore, _ *StoreHandler) {
			var b map[string]interface{}
			if body != nil {
				b = body()
			}
			c.provision(t, action, b)
		}}
	}
	manualActive := op{"operator sets active", func(t *testing.T, c *capsStore, h *StoreHandler) {
		if err := h.d.Billing.Reactivate("u1"); err != nil {
			t.Fatal(err)
		}
	}}
	manualPastDue := op{"operator sets past_due", func(t *testing.T, c *capsStore, h *StoreHandler) {
		if err := h.d.Billing.EnterPastDue("u1"); err != nil {
			t.Fatal(err)
		}
	}}
	ended := func() map[string]interface{} { return map[string]interface{}{"entitlementEnded": true} }
	cases := []struct {
		name string
		ops  []op
		// want is the view after each op.
		want []managedView
	}{
		{"plan end, then operator active", []op{provision("activate", managedBody), provision("suspend", ended), manualActive},
			[]managedView{all, none, none}},
		{"plan end, then operator past_due", []op{provision("activate", managedBody), provision("suspend", ended), manualPastDue},
			[]managedView{all, none, none}},
		{"plan end after a cutoff, then operator active", []op{provision("activate", managedBody), provision("suspend", nil), provision("suspend", ended), manualActive},
			[]managedView{all, all, none, none}},
		{"cutoff, then the status alone comes back", []op{provision("activate", managedBody), provision("suspend", nil), manualActive},
			[]managedView{all, all, all}},
		{"cutoff, then the store's release activate", []op{provision("activate", managedBody), provision("suspend", nil), provision("activate", managedBody)},
			[]managedView{all, all, all}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapsStore()
			c.beamKeys = []models.BeamIssuerKey{{ID: testBeamKid, OwnerID: "u1", PublicKey: make([]byte, 32)}}
			h := &StoreHandler{d: Deps{Store: c, Redis: newStoreLinkRedis(t), StoreEnabled: true, StoreSharedKey: storeLinkTestKey, SuspendGrace: 48 * time.Hour}}
			h.d.Billing = services.NewBillingLifecycleService(c, services.NewQueueService(h.d.Redis), nil, "https://panel.example.com", 48*time.Hour, true)
			for i, o := range tc.ops {
				o.run(t, c, h)
				if got := c.view(t, h); got != tc.want[i] {
					t.Fatalf("after %q (step %d): %+v, want %+v", o.name, i, got, tc.want[i])
				}
			}
			if final := tc.ops[len(tc.ops)-1].name; final == "operator sets active" && c.billingStatus != "active" {
				t.Fatalf("status %q, want active", c.billingStatus)
			}
		})
	}
}
