package services

import (
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type notifyFakeStore struct {
	billing []store.UserBilling
	// own rows keyed "user:<id>|region|kind", for the tenants who have one.
	own  map[string]*models.TrafficLimit
	sent []models.Notification
}

func (f *notifyFakeStore) ListUserBilling() ([]store.UserBilling, error) { return f.billing, nil }

func (f *notifyFakeStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	return f.own[scope+"|"+region+"|"+kind], nil
}

func (f *notifyFakeStore) InsertNotification(n *models.Notification) (int64, error) {
	f.sent = append(f.sent, *n)
	return int64(len(f.sent)), nil
}

func (f *notifyFakeStore) to(userID string) *models.Notification {
	for i := range f.sent {
		if f.sent[i].UserID == userID {
			return &f.sent[i]
		}
	}
	return nil
}

func unit(n int64) *int64 { return &n }

func billed(id string, traffic, backup bool, units int64) store.UserBilling {
	return store.UserBilling{
		UserID:                id,
		MaxNodes:              &units,
		TrafficBillingEnabled: traffic,
		BackupBillingEnabled:  backup,
	}
}

// Only the tenants who agreed to be charged hear about it.
//
// The notification exists because metered billing is a consent to a KNOWN
// maximum. A tenant with it OFF is not being charged and this number does not
// apply to them, so telling them would be an alarm about nothing.
func TestBackupBookableNotifiesOnlyConsentingTenants(t *testing.T) {
	st := &notifyFakeStore{billing: []store.UserBilling{
		billed("consented", false, true, 1),
		billed("not-consented", false, false, 1),
		billed("traffic-only", true, false, 1),
	}}
	if n := NotifyBackupBookableChanged(st, 500, 800); n != 1 {
		t.Fatalf("notified %d tenants, want 1", n)
	}
	if got := st.to("consented"); got == nil {
		t.Fatal("the tenant who agreed to be charged was not told")
	} else if !strings.Contains(got.Body, "500 GB") || !strings.Contains(got.Body, "800 GB") {
		t.Errorf("body does not state both numbers: %q", got.Body)
	}
}

// The tenant is told THEIR number, not the per-unit setting. A customer holding
// two units reads 1600, which is the figure their own screen shows and the one
// their bill stops at.
func TestBackupBookableStatesTheTenantsOwnNumber(t *testing.T) {
	st := &notifyFakeStore{billing: []store.UserBilling{billed("two-units", false, true, 2)}}
	NotifyBackupBookableChanged(st, 500, 800)
	got := st.to("two-units")
	if got == nil {
		t.Fatal("not notified")
	}
	if !strings.Contains(got.Body, "1000 GB") || !strings.Contains(got.Body, "1600 GB") {
		t.Errorf("body should be scaled to two units: %q", got.Body)
	}
}

// A save that does not move the number tells nobody. The billing screen carries
// eight settings and an operator editing the payment URL saves all of them.
func TestBookableUnchangedNotifiesNobody(t *testing.T) {
	st := &notifyFakeStore{billing: []store.UserBilling{billed("u1", true, true, 1)}}
	if n := NotifyBackupBookableChanged(st, 500, 500); n != 0 {
		t.Errorf("notified %d tenants for an unchanged value", n)
	}
	if n := NotifyTrafficPurchaseChanged(st, TrafficScopeDefault, "eu-central", TrafficKindEdge, unit(2000), unit(2000)); n != 0 {
		t.Errorf("notified %d tenants for an unchanged traffic cap", n)
	}
	if n := NotifyTrafficPurchaseChanged(st, TrafficScopeDefault, "eu-central", TrafficKindEdge, nil, nil); n != 0 {
		t.Errorf("notified %d tenants for two absent caps", n)
	}
	if len(st.sent) != 0 {
		t.Errorf("wrote %d notifications", len(st.sent))
	}
}

// The tenant default reaches only the tenants the resolver would ask it about.
//
// One with their own row is answered by that row, so the default moving changes
// nothing for them - and a notification saying their ceiling moved when it did
// not is worse than silence.
func TestTrafficDefaultSkipsTenantsWithTheirOwnRow(t *testing.T) {
	st := &notifyFakeStore{
		billing: []store.UserBilling{
			billed("uses-default", true, false, 1),
			billed("has-own-row", true, false, 1),
		},
		own: map[string]*models.TrafficLimit{
			"user:has-own-row|eu-central|edge": {MaxPurchaseGB: unit(9000)},
		},
	}
	if n := NotifyTrafficPurchaseChanged(st, TrafficScopeDefault, "eu-central", TrafficKindEdge, unit(2000), unit(3000)); n != 1 {
		t.Fatalf("notified %d tenants, want 1", n)
	}
	if st.to("has-own-row") != nil {
		t.Error("a tenant with their own row was told the default moved")
	}
	got := st.to("uses-default")
	if got == nil {
		t.Fatal("the tenant on the default was not told")
	}
	if !strings.Contains(got.Body, "player traffic (eu-central)") {
		t.Errorf("body does not name the pool in words: %q", got.Body)
	}
}

// A per-user row concerns exactly that user.
func TestTrafficUserScopeReachesOnlyThatUser(t *testing.T) {
	st := &notifyFakeStore{billing: []store.UserBilling{
		billed("target", true, false, 1),
		billed("bystander", true, false, 1),
	}}
	if n := NotifyTrafficPurchaseChanged(st, "user:target", "eu-central", TrafficKindEdge, unit(1000), nil); n != 1 {
		t.Fatalf("notified %d tenants, want 1", n)
	}
	if st.to("bystander") != nil {
		t.Error("a bystander was told about someone else's override")
	}
	// nil is no cap, which cannot be written as a quantity and is the state a
	// customer most needs told apart from a large number.
	if got := st.to("target"); got == nil || !strings.Contains(got.Body, "no limit") {
		t.Errorf("removing the cap should say so in words: %+v", got)
	}
}

// A tenant holding nothing has no bookable allowance either way, so the change
// is from zero to zero and there is nothing to report.
func TestBookableSkipsTenantsHoldingNothing(t *testing.T) {
	st := &notifyFakeStore{billing: []store.UserBilling{
		{UserID: "no-units", BackupBillingEnabled: true, TrafficBillingEnabled: true},
	}}
	if n := NotifyBackupBookableChanged(st, 500, 800); n != 0 {
		t.Errorf("notified %d tenants who hold nothing", n)
	}
	if n := NotifyTrafficPurchaseChanged(st, TrafficScopeDefault, "eu-central", TrafficKindEdge, unit(1000), unit(2000)); n != 0 {
		t.Errorf("notified %d tenants who hold nothing", n)
	}
}

// File transfers hold one pool for every region, so their label carries no
// region: a literal "*" in front of a customer means nothing to them.
func TestTrafficPoolLabel(t *testing.T) {
	tests := []struct{ region, kind, want string }{
		{"eu-central", TrafficKindEdge, "player traffic (eu-central)"},
		{TrafficRegionAny, TrafficKindRelay, "file transfer"},
		{"", TrafficKindEdge, "player traffic"},
		{"eu-central", "warp", "warp (eu-central)"},
	}
	for _, tt := range tests {
		if got := trafficPoolLabel(tt.region, tt.kind); got != tt.want {
			t.Errorf("trafficPoolLabel(%q, %q) = %q, want %q", tt.region, tt.kind, got, tt.want)
		}
	}
}

// Traffic is granted per unit HELD, nodes and route-only links alike
// (TrafficUnits), and a user's own "*" row answers every region for them. The
// notice counted nodes only and checked only an exact row.
func TestTrafficPurchaseNoticeCountsEveryUnit(t *testing.T) {
	two, one := int64(2), int64(1)
	cases := []struct {
		name   string
		b      store.UserBilling
		own    map[string]*models.TrafficLimit
		want   bool
		wantIn []string
	}{
		{"route-only tenant", store.UserBilling{UserID: "u", MaxLinks: &two, TrafficBillingEnabled: true}, nil, true,
			[]string{"1000 GB", "200 GB"}},
		{"node plus two links", store.UserBilling{UserID: "u", MaxNodes: &one, MaxLinks: &two, TrafficBillingEnabled: true}, nil, true,
			[]string{"1500 GB", "300 GB"}},
		{"managed tenant answered by their own * row", store.UserBilling{UserID: "u", MaxLinks: &two, TrafficBillingEnabled: true},
			map[string]*models.TrafficLimit{"user:u|*|" + TrafficKindEdge: {MaxPurchaseGB: unit(5000)}}, false, nil},
		{"holds nothing", store.UserBilling{UserID: "u", TrafficBillingEnabled: true}, nil, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &notifyFakeStore{billing: []store.UserBilling{tc.b}, own: tc.own}
			NotifyTrafficPurchaseChanged(st, TrafficScopeDefault, "eu-central", TrafficKindEdge, unit(500), unit(100))
			got := st.to("u")
			if (got != nil) != tc.want {
				t.Fatalf("notified = %v, want %v", got != nil, tc.want)
			}
			for _, s := range tc.wantIn {
				if !strings.Contains(got.Body, s) {
					t.Errorf("body %q does not contain %q", got.Body, s)
				}
			}
		})
	}
}
