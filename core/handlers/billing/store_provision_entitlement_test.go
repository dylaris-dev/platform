package billing

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"dylaris-core/models"
)

// TestProvision_PurchasedEntitlement pins the half of the store contract that was
// missing until 2026-08-23: the store sells a node COUNT, not one of Core's plans,
// so the number the customer paid for arrives as maxNodes/maxLinks and has to land
// as a per-user override. Without this the tenant kept the DEFAULT plan's limits -
// a customer who bought five nodes was capped at the free tier's one.
func TestProvision_PurchasedEntitlement(t *testing.T) {
	nodes := func(n int64) *int64 { return &n }

	tests := []struct {
		name     string
		body     map[string]interface{}
		wantCall bool
		want     storeLinkEntitlementCall
	}{
		{
			name:     "node count lands as a maxNodes override",
			body:     map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": 5},
			wantCall: true,
			want:     storeLinkEntitlementCall{"u1", nodes(5), true, nil, false},
		},
		{
			name:     "a manual grant carries routes too",
			body:     map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": 3, "maxLinks": 10},
			wantCall: true,
			want:     storeLinkEntitlementCall{"u1", nodes(3), true, nodes(10), true},
		},
		{
			// 0 in user_billing means UNLIMITED, so a "0 nodes" grant must clear the
			// override rather than be written through as the literal value.
			name:     "zero clears the override instead of meaning unlimited",
			body:     map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": 0},
			wantCall: true,
			want:     storeLinkEntitlementCall{"u1", nil, true, nil, false},
		},
		{
			name:     "explicit null clears the override",
			body:     map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": nil},
			wantCall: true,
			want:     storeLinkEntitlementCall{"u1", nil, true, nil, false},
		},
		{
			// An older store that has not learned the field yet must not have its
			// tenants' overrides wiped on every renewal.
			name:     "an omitted field touches nothing",
			body:     map[string]interface{}{"uuid": "u1", "action": "activate"},
			wantCall: false,
		},
		{
			name:     "suspend never rewrites the entitlement",
			body:     map[string]interface{}{"uuid": "u1", "action": "suspend", "maxNodes": 5},
			wantCall: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
			h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
			rec := httptest.NewRecorder()
			h.Provision(rec, storeLinkPost("/api/store/provision", tt.body, storeLinkTestKey))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if !tt.wantCall {
				if len(fs.entitlementCalls) != 0 {
					t.Fatalf("entitlementCalls = %+v, want none", fs.entitlementCalls)
				}
				return
			}
			if len(fs.entitlementCalls) != 1 {
				t.Fatalf("entitlementCalls = %+v, want exactly 1", fs.entitlementCalls)
			}
			got := fs.entitlementCalls[0]
			if got.userID != tt.want.userID || got.setNodes != tt.want.setNodes || got.setLinks != tt.want.setLinks ||
				!eqInt64Ptr(got.maxNodes, tt.want.maxNodes) || !eqInt64Ptr(got.maxLinks, tt.want.maxLinks) {
				t.Fatalf("call = %s, want %s", fmtEntitlement(got), fmtEntitlement(tt.want))
			}
		})
	}
}

// TestProvision_EntitlementWriteFails proves a failed override write is reported
// rather than swallowed: the tenant is active but under the wrong cap, and the
// store must see a 500 so the failure is visible in its logs.
func TestProvision_EntitlementWriteFails(t *testing.T) {
	fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}, entitlementErr: errStoreLinkEntitlement}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": 5}, storeLinkTestKey))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

func eqInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtEntitlement(c storeLinkEntitlementCall) string {
	f := func(p *int64) string {
		if p == nil {
			return "nil"
		}
		return itoa64(*p)
	}
	return "{" + c.userID + " nodes=" + f(c.maxNodes) + " setNodes=" + btoa(c.setNodes) +
		" links=" + f(c.maxLinks) + " setLinks=" + btoa(c.setLinks) + "}"
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

func btoa(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

var errStoreLinkEntitlement = errors.New("db down")

// The purchase has to actually RETIRE the grant through the provision path.
//
// The logic is unit-tested next door; this pins the WIRING, which is the half
// that broke: retireGrantsCoveredByPurchase was added to Provision and the fake
// store here did not implement the method it calls, so the whole endpoint
// panicked on every activate. A test that only exercises the helper cannot see
// that.
func TestProvisionRetiresTheGrantItCovers(t *testing.T) {
	tests := []struct {
		name string
		body map[string]interface{}
		want []string
	}{
		{
			name: "buying nodes retires the BYON grant",
			body: map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": 2},
			want: []string{"byon"},
		},
		{
			name: "buying both retires both",
			body: map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": 1, "maxLinks": 1},
			want: []string{"byon", "route_only"},
		},
		{
			// A cancellation pushes zero, which parses to "clear the override".
			// It must not take away a grant an admin gave separately.
			name: "a cancellation retires nothing",
			body: map[string]interface{}{"uuid": "u1", "action": "activate", "maxNodes": 0},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
			h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
			rec := httptest.NewRecorder()
			h.Provision(rec, storeLinkPost("/api/store/provision", tt.body, storeLinkTestKey))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if len(fs.retiredGrants) != len(tt.want) {
				t.Fatalf("retired %v, want %v", fs.retiredGrants, tt.want)
			}
			for i, k := range tt.want {
				if fs.retiredGrants[i] != k {
					t.Errorf("retired[%d] = %q, want %q", i, fs.retiredGrants[i], k)
				}
			}
		})
	}
}
