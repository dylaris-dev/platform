package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// An operator's ceiling replaces the purchase's number and nothing else: it
// never gives access, and an empty one follows the purchase again.
func TestAnOperatorsCeilingSitsOverThePurchase(t *testing.T) {
	one, three := int64(1), int64(3)
	cases := []struct {
		name      string
		b         store.UserBilling
		wantNodes string
		wantByon  bool
	}{
		{"empty follows the purchase", store.UserBilling{Status: "active", MaxNodes: &three}, "3", true},
		{"a number replaces it", store.UserBilling{Status: "active", MaxNodes: &one, AdminMaxNodes: "5"}, "5", true},
		{"unlimited lifts it", store.UserBilling{Status: "active", MaxNodes: &one, AdminMaxNodes: LimitUnlimited}, "none", true},
		{"zero is a ceiling of none and keeps the access", store.UserBilling{Status: "active", MaxNodes: &three, AdminMaxNodes: "0"}, "0", true},
		{"a ceiling without a purchase grants nothing", store.UserBilling{Status: "active", AdminMaxNodes: "5"}, "5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeLimitStore{billing: &tc.b}
			lim, err := EffectiveLimits(st, "u1")
			if err != nil {
				t.Fatal(err)
			}
			got := "none"
			if lim.MaxNodes != nil {
				got = FormatLimitSetting(lim.MaxNodes)
			}
			if got != tc.wantNodes {
				t.Errorf("node ceiling = %s, want %s", got, tc.wantNodes)
			}
			ent, err := EffectiveEntitlement(st, "u1", time.Now(), true, false)
			if err != nil {
				t.Fatal(err)
			}
			if ent.Byon != tc.wantByon {
				t.Errorf("byon = %v, want %v", ent.Byon, tc.wantByon)
			}
		})
	}
}

type scopedRouteStore struct {
	*overLimitStore
	rows map[string]*models.GatewayRouteLimit
}

func (s scopedRouteStore) GetGatewayRouteLimit(scope string) (*models.GatewayRouteLimit, error) {
	return s.rows[scope], nil
}

// The over-limit sweep reads the address ceiling the way the create paths do:
// the operator's row first, then what the store sold.
func TestTheSweepReadsTheOperatorsAddressCeilingFirst(t *testing.T) {
	n := func(v int) *int { return &v }
	cases := []struct {
		name string
		rows map[string]*models.GatewayRouteLimit
		want string
	}{
		{"purchase only", map[string]*models.GatewayRouteLimit{"user:u1": {MaxRoutes: n(7)}}, "7"},
		{"operator over purchase", map[string]*models.GatewayRouteLimit{"admin:u1": {MaxRoutes: n(2)}, "user:u1": {MaxRoutes: n(7)}}, "2"},
		{"operator no-cap over a purchase of none", map[string]*models.GatewayRouteLimit{"admin:u1": {MaxRoutes: nil}, "user:u1": {MaxRoutes: n(0)}}, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := scopedRouteStore{overLimitStore: &overLimitStore{billing: []store.UserBilling{{UserID: "u1", Status: "active"}}}, rows: tc.rows}
			u, err := (&BillingLifecycleService{store: st}).tenantUsageFor(context.Background(), "u1")
			if err != nil {
				t.Fatal(err)
			}
			got := "none"
			if u.routeLimit != nil {
				got = FormatLimitSetting(u.routeLimit)
			}
			if got != tc.want {
				t.Errorf("route ceiling = %s, want %s", got, tc.want)
			}
		})
	}
}
