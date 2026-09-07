package services

import (
	"testing"

	"dylaris-core/store"
)

// Reuses quotaFakeStore from backup_quota_nodelocal_test.go: it answers only
// GetSetting and panics on everything else, which is what keeps these tests
// about the resolution and nothing beside it.

func i64(n int64) *int64 { return &n }

// The allowance scales with what was BOUGHT, like every other one here.
//
// It used to be one number for the whole account, so a customer with three BYON
// units got three times the addresses, three times the traffic and one times the
// backup storage. That was the only allowance that did not follow the purchase.
func TestR2QuotaScalesWithUnits(t *testing.T) {
	st := &quotaFakeStore{kv: map[string]string{}} // defaults: 50 included, 500 bookable

	tests := []struct {
		name    string
		billing *store.UserBilling
		want    *int64
	}{
		{
			name:    "one BYON unit, billing off",
			billing: &store.UserBilling{MaxNodes: i64(1)},
			want:    i64(50),
		},
		{
			name:    "three units, billing off",
			billing: &store.UserBilling{MaxNodes: i64(3)},
			want:    i64(150),
		},
		{
			// Only the node brings backup storage. A route-only location is a
			// route to a server the customer runs themselves, so there is
			// nothing of theirs here to back up - it used to bring a full
			// node's allowance anyway, so a customer holding one of each was
			// given twice what their node includes.
			name:    "a node and a route-only location count as the node alone",
			billing: &store.UserBilling{MaxNodes: i64(1), MaxLinks: i64(1)},
			want:    i64(50),
		},
		{
			// Consent raises the ceiling; it does not raise the included amount.
			// What is over 50 is billed, what is over 550 is refused.
			name:    "one unit with backup billing on",
			billing: &store.UserBilling{MaxNodes: i64(1), BackupBillingEnabled: true},
			want:    i64(550),
		},
		{
			name:    "two units with backup billing on",
			billing: &store.UserBilling{MaxNodes: i64(2), BackupBillingEnabled: true},
			want:    i64(1100),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st.billing = tt.billing
			got := BackupAllowanceGB(st, "owner-1", true)
			if got == nil || tt.want == nil {
				t.Fatalf("quota = %v, want %v", got, tt.want)
			}
			if *got != *tt.want {
				t.Errorf("quota = %d, want %d", *got, *tt.want)
			}
		})
	}
}

// Route-only on its own brings no backup allowance at all, so the resolution
// carries on to the operator's setting instead of stopping at a node's worth.
func TestRouteOnlyBringsNoBackupAllowance(t *testing.T) {
	st := &quotaFakeStore{
		kv:      map[string]string{SettingBackupDefaultUserQuota: "7"},
		billing: &store.UserBilling{MaxLinks: i64(2)},
	}
	if got := BackupAllowanceGB(st, "owner-1", true); got == nil || *got != 7 {
		t.Fatalf("quota = %v, want the operator allowance of 7", got)
	}
	// And with no operator allowance either: no cap, rather than a quota of
	// zero. A route-only customer must not be worse off than an unknown one.
	bare := &quotaFakeStore{kv: map[string]string{}, billing: &store.UserBilling{MaxLinks: i64(2)}}
	if got := BackupAllowanceGB(bare, "owner-1", true); got != nil {
		t.Fatalf("quota = %v, want nil", got)
	}
}

// A per-user override still wins, and it wins over the purchase too.
//
// That is the point of an override: an operator raising one tenant's ceiling by
// hand must not have it silently recomputed from the units they hold.
func TestR2QuotaPerUserOverrideWins(t *testing.T) {
	st := &quotaFakeStore{kv: map[string]string{}}
	st.billing = &store.UserBilling{MaxNodes: i64(3), R2QuotaGB: i64(10), BackupBillingEnabled: true}
	got := BackupAllowanceGB(st, "owner-1", true)
	if got == nil || *got != 10 {
		t.Fatalf("quota = %v, want the override of 10", got)
	}
}

// A tenant who bought nothing falls through to the flat platform setting.
//
// Returning a purchase-derived zero instead would stop backups on every
// self-hosted install the moment this shipped - nobody there buys units, and
// every one of them would have been handed a quota of none.
func TestR2QuotaWithoutAPurchaseFallsThrough(t *testing.T) {
	st := &quotaFakeStore{kv: map[string]string{SettingBackupDefaultUserQuota: "250"}}

	if got := BackupAllowanceGB(st, "owner-1", true); got == nil || *got != 250 {
		t.Fatalf("no billing row: quota = %v, want the operator allowance of 250", got)
	}
	st.billing = &store.UserBilling{}
	if got := BackupAllowanceGB(st, "owner-1", true); got == nil || *got != 250 {
		t.Fatalf("a row with no units: quota = %v, want 250", got)
	}
	// And with no allowance set either, no cap at all - not a cap of zero.
	bare := &quotaFakeStore{kv: map[string]string{}, billing: &store.UserBilling{}}
	if got := BackupAllowanceGB(bare, "owner-1", true); got != nil {
		t.Fatalf("quota = %v, want nil (no cap configured anywhere)", got)
	}
}

// The settings are what an operator edits, so they have to actually apply.
func TestR2QuotaHonoursTheSettings(t *testing.T) {
	st := &quotaFakeStore{kv: map[string]string{
		BillingR2IncludedKey: "100",
		BillingR2BookableKey: "200",
	}}
	st.billing = &store.UserBilling{MaxNodes: i64(2)}
	if got := BackupAllowanceGB(st, "owner-1", true); got == nil || *got != 200 {
		t.Fatalf("billing off: quota = %v, want 200", got)
	}
	st.billing.BackupBillingEnabled = true
	if got := BackupAllowanceGB(st, "owner-1", true); got == nil || *got != 600 {
		t.Fatalf("billing on: quota = %v, want 600", got)
	}
}

// A stored zero for the included amount is a real answer: this product includes
// no backup storage. It must not fall back to the built-in 50.
func TestR2IncludedZeroIsACap(t *testing.T) {
	st := &quotaFakeStore{kv: map[string]string{BillingR2IncludedKey: "0"}}
	b := &store.UserBilling{MaxNodes: i64(2)}
	if got := R2IncludedGB(st, b); got != 0 {
		t.Errorf("included = %d, want 0 - a stored zero is a decision", got)
	}
}
