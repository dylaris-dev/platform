package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// An operator's hold writes status "suspended" and its own suspended_at over
// whatever the payments say; nothing is deleted while it lasts.
func TestCleanupExpiredR2SparesAnOperatorsHold(t *testing.T) {
	old := timePtr(time.Now().Add(-30 * 24 * time.Hour))
	cases := []struct {
		name        string
		b           store.UserBilling
		wantCleaned bool
	}{
		{"held paying customer", store.UserBilling{UserID: "u1", Status: "suspended", SuspendedAt: old, R2Retention: "1d", AdminHold: true, HeldStatus: "active"}, false},
		{"held past_due customer", store.UserBilling{UserID: "u1", Status: "suspended", SuspendedAt: old, R2Retention: "1d", AdminHold: true, HeldStatus: "past_due"}, false},
		{"held over a non-payment suspension", store.UserBilling{UserID: "u1", Status: "suspended", SuspendedAt: old, R2Retention: "1d", AdminHold: true, HeldStatus: "suspended"}, false},
		{"suspended for non-payment", store.UserBilling{UserID: "u1", Status: "suspended", SuspendedAt: old, R2Retention: "1d"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &billingFakeStore{suspended: []store.UserBilling{tc.b}}
			(&BillingLifecycleService{store: fs}).cleanupExpiredR2(context.Background())
			if cleaned := len(fs.listBackupsCalls) == 1; cleaned != tc.wantCleaned {
				t.Errorf("cleaned = %v, want %v", cleaned, tc.wantCleaned)
			}
		})
	}
}

type sftpCutoffStore struct {
	store.Store
	b *store.UserBilling
}

func (s sftpCutoffStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, OwnerID: "u1"}, nil
}
func (s sftpCutoffStore) GetUserBilling(string) (*store.UserBilling, error) { return s.b, nil }

// SFTP takes a cut-off owner's servers away like every HTTP write path refuses
// them: for non-payment, and for holding more than was bought past its grace.
func TestSFTPDropsACutOffOwner(t *testing.T) {
	cases := []struct {
		name string
		b    *store.UserBilling
		want bool
	}{
		{"no billing row", nil, false},
		{"active", &store.UserBilling{Status: "active"}, false},
		{"suspended", &store.UserBilling{Status: "suspended"}, true},
		{"over limit, still in grace", &store.UserBilling{Status: "active", OverLimitSince: timePtr(time.Now().Add(-time.Hour))}, false},
		{"over limit past grace", &store.UserBilling{Status: "active", OverLimitSince: timePtr(time.Now().Add(-OverLimitGrace - time.Hour))}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newOwnerSuspension(sftpCutoffStore{b: tc.b}).of(1); got != tc.want {
				t.Errorf("cut off = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBillingEmailLinksAndRetention(t *testing.T) {
	fs := &billingFakeStore{settings: map[string]string{BillingR2RetentionKey: "1w"}}
	svc := &BillingLifecycleService{store: fs, frontendURL: "https://panel.example/"}
	// /account/billing never existed; every banner and email linked to a 404.
	if got := svc.paymentURL(); got != "https://panel.example/account/store" {
		t.Errorf("payment url = %q", got)
	}
	if got := svc.backupRetentionText("u1"); got != "1 week" {
		t.Errorf("retention = %q, want 1 week", got)
	}
	fs.settings[BillingR2RetentionKey] = "3d"
	if got := svc.backupRetentionText("u1"); got != "3 days" {
		t.Errorf("retention = %q, want 3 days", got)
	}
}
