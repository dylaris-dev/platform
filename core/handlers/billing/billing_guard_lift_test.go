package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type liftHoldStore struct {
	billingGuardStore
	prior    string
	statuses []string
}

func (f *liftHoldStore) LiftAdminHold(string) (string, error)                 { return f.prior, nil }
func (f *liftHoldStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }
func (f *liftHoldStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID, Status: "suspended"}, nil
}
func (f *liftHoldStore) SetUserBillingStatus(_ string, status string, _, _ *time.Time) error {
	f.statuses = append(f.statuses, status)
	return nil
}
func (f *liftHoldStore) SetUserBillingStatusIf(_ string, status string, _, _ *time.Time, _ []string) (bool, error) {
	f.statuses = append(f.statuses, status)
	return true, nil
}

// Lifting an operator's hold returns the account to what its payments say. An
// operator choosing "active" for a held account that had stopped paying used to
// make it active, unpaid, for good.
func TestLiftingAHoldReturnsToWhatThePaymentsSay(t *testing.T) {
	for _, tc := range []struct {
		name, prior, requested string
		wantWrites             []string
		wantStatus             string
	}{
		{"stopped paying while held", "suspended", "active", nil, "suspended"},
		{"paid up underneath", "active", "past_due", []string{"active"}, "active"},
		{"no hold: the operator's choice stands", "", "active", []string{"active"}, "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &liftHoldStore{billingGuardStore: billingGuardStore{target: &models.User{ID: "u1", Username: "tenant"}}, prior: tc.prior}
			rdb := newServerPowerRedis(t)
			state := &handlers.AppState{Store: fs, Redis: rdb}
			state.Billing = services.NewBillingLifecycleService(fs, services.NewQueueService(rdb), nil, "https://panel.example.com", 48*time.Hour, true)
			h := &BillingHandler{state: state}

			req := httptest.NewRequest("PATCH", "/api/admin/users/u1/billing", strings.NewReader(`{"status":"`+tc.requested+`"}`))
			req = mux.SetURLVars(req, map[string]string{"id": "u1"})
			ctx := context.WithValue(req.Context(), "username", "admin")
			ctx = context.WithValue(ctx, "isAdmin", true)
			ctx = context.WithValue(ctx, "userID", "admin-1")
			rec := httptest.NewRecorder()
			h.SetBillingStatus(rec, req.WithContext(ctx))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if strings.Join(fs.statuses, ",") != strings.Join(tc.wantWrites, ",") {
				t.Fatalf("writes %v, want %v", fs.statuses, tc.wantWrites)
			}
			if !strings.Contains(rec.Body.String(), `"status":"`+tc.wantStatus+`"`) {
				t.Fatalf("the operator was not told where the account went: %s", rec.Body.String())
			}
		})
	}
}
