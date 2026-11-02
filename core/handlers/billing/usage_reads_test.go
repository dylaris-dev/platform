package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// usageCountStore serves one tenant and counts the rows a usage response reads.
type usageCountStore struct {
	store.Store
	user         *models.User
	billing      *store.UserBilling
	billingReads int
	userReads    int
}

func (f *usageCountStore) GetTrafficUsage(userID string, _ time.Time) (*store.TrafficUsage, error) {
	return &store.TrafficUsage{UserID: userID, BackupBytes: 3 * usageGiB}, nil
}
func (f *usageCountStore) GetUserBilling(string) (*store.UserBilling, error) {
	f.billingReads++
	if f.billing == nil {
		return nil, nil
	}
	cp := *f.billing
	return &cp, nil
}
func (f *usageCountStore) GetUserByID(string) (*models.User, error) {
	f.userReads++
	cp := *f.user
	return &cp, nil
}
func (f *usageCountStore) GetSetting(string) (string, error) { return "", nil }

// GET /api/me/usage reads the billing row once and the user row not at all
// (AuthMiddleware already did), and answers exactly what separate reads
// answered: the over flags and the over-limit banner still fire.
func TestMyUsageReadsTheBillingRowOnce(t *testing.T) {
	quota := int64(2)
	since := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name      string
		billing   *store.UserBilling
		wantOver  bool
		wantState bool
	}{
		{"over its backup quota and over limit", &store.UserBilling{UserID: "u1", R2QuotaGB: &quota, OverLimitSince: &since}, true, true},
		{"no billing row", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &usageCountStore{user: &models.User{ID: "u1", Username: "alice"}, billing: tc.billing}
			h := NewUsageHandler(Deps{Store: fs})
			req := httptest.NewRequest(http.MethodGet, "/api/me/usage", nil)
			ctx := context.WithValue(req.Context(), "userID", "u1")
			ctx = authz.WithUser(ctx, fs.user)
			rec := httptest.NewRecorder()
			h.GetMyUsage(rec, req.WithContext(ctx))

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if fs.billingReads != 1 || fs.userReads != 0 {
				t.Fatalf("billing reads %d, user reads %d; want 1 and 0", fs.billingReads, fs.userReads)
			}
			body := rec.Body.String()
			if got := strings.Contains(body, `"r2":true`); got != tc.wantOver {
				t.Errorf("r2 over = %v, want %v: %s", got, tc.wantOver, body)
			}
			if got := strings.Contains(body, `"entitlementState"`); got != tc.wantState {
				t.Errorf("entitlementState present = %v, want %v: %s", got, tc.wantState, body)
			}
		})
	}
}

// The cache is one tenant's: another id still goes to the store.
func TestTenantReadsOnlyServeTheirTenant(t *testing.T) {
	fs := &usageCountStore{user: &models.User{ID: "u1"}, billing: &store.UserBilling{UserID: "u1"}}
	tr := &tenantReads{Store: fs, userID: "u1", user: fs.user}
	tr.GetUserBilling("u1")
	tr.GetUserBilling("u1")
	tr.GetUserBilling("u2")
	tr.GetUserByID("u2")
	if fs.billingReads != 2 || fs.userReads != 1 {
		t.Fatalf("billing %d user %d, want 2 and 1", fs.billingReads, fs.userReads)
	}
}
