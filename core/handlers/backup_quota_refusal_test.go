package handlers

import (
	"errors"
	"strings"
	"testing"
)

// A cap of zero has to say so. The wording used to be one template for both
// situations, so an allowance of none came out as "(0 / 0 GB used) - delete old
// backups or raise the limit": two numbers that look like a fault and one
// instruction that cannot work, since deleting backups frees nothing against a
// quota of nothing.
//
// MEASURED in production before this: settings.billing.r2_quota_gb held "0",
// written by an older settings screen that echoed its own placeholder back on
// save, and every server whose owner held no entitlement got exactly that
// message.
func TestBackupQuotaRefusal(t *testing.T) {
	const gb = 1024 * 1024 * 1024

	tests := []struct {
		name        string
		used, quota int64
		scope       string
		wantSubs    []string
		notSubs     []string
	}{
		{
			name: "a cap of zero names the allowance, not the usage",
			used: 0, quota: 0,
			wantSubs: []string{"allowance is 0 GB", "no backup can be stored", "raise the limit"},
			// The instruction that cannot be followed must be gone.
			notSubs: []string{"delete old backups", "0 / 0"},
		},
		{
			name: "a full quota keeps the figures and the advice",
			used: 12 * gb, quota: 10 * gb,
			wantSubs: []string{"12.0 / 10.0 GB used", "delete old backups"},
			notSubs:  []string{"allowance"},
		},
		{
			// Rendered as integers before, so anything under a gigabyte read as
			// "0 / 1 GB used" - which is the same unactionable shape as the bug
			// above, one order of magnitude down.
			name: "sub-gigabyte usage is visible",
			used: 512 * 1024 * 1024, quota: 1 * gb,
			wantSubs: []string{"0.5 / 1.0 GB used"},
		},
		{
			name: "the scope reaches both wordings",
			used: 0, quota: 0, scope: " on this server",
			wantSubs: []string{"allowance on this server is 0 GB"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := backupQuotaRefusal(tt.used, tt.quota, tt.scope, "raise the limit")
			// The sentinel is what turns the refusal into a 409 rather than a
			// 500, so every branch has to carry it.
			if !errors.Is(err, errBackupQuotaReached) {
				t.Fatalf("error does not wrap errBackupQuotaReached: %v", err)
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("missing %q in %q", sub, err.Error())
				}
			}
			for _, sub := range tt.notSubs {
				if strings.Contains(err.Error(), sub) {
					t.Errorf("unexpected %q in %q", sub, err.Error())
				}
			}
		})
	}
}
