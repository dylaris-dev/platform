package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type billingGuardStore struct {
	store.Store
	target *models.User
}

func (f *billingGuardStore) GetUserByID(string) (*models.User, error) { return f.target, nil }

// plans.write is not an admin capability, and a suspension stops every server
// the account owns and revokes its link kits for good. It was the one account
// action a holder could aim at an admin.
func TestBillingStatusCannotBeAimedAtAStrongerAccount(t *testing.T) {
	fs := &billingGuardStore{target: &models.User{ID: "admin-1", Username: "boss", IsAdmin: true, Role: "admin"}}
	rdb := newServerPowerRedis(t)
	state := &AppState{Store: fs, Redis: rdb}
	state.Billing = services.NewBillingLifecycleService(fs, services.NewQueueService(rdb), nil, "https://panel.example.com", 48*time.Hour, true)
	h := &BillingHandler{state: state}

	req := httptest.NewRequest("PATCH", "/api/admin/users/admin-1/billing", strings.NewReader(`{"status":"suspended"}`))
	req = mux.SetURLVars(req, map[string]string{"id": "admin-1"})
	ctx := context.WithValue(req.Context(), "username", "billing-staff")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", "staff-1")
	rec := httptest.NewRecorder()
	h.SetBillingStatus(rec, req.WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a non-admin suspending an admin: status %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

type billingHoldStore struct {
	billingGuardStore
	holds []bool
}

func (f *billingHoldStore) SetUserBillingAdminHold(_ string, hold bool) error {
	f.holds = append(f.holds, hold)
	return nil
}

func (f *billingHoldStore) SetUserBillingStatus(string, string, *time.Time, *time.Time) error {
	return errors.New("stop here")
}

// The operator's own way out of their suspension lifts the hold that keeps the
// store from doing it.
func TestAnOperatorReactivationLiftsTheHold(t *testing.T) {
	fs := &billingHoldStore{billingGuardStore: billingGuardStore{target: &models.User{ID: "u1", Username: "tenant"}}}
	rdb := newServerPowerRedis(t)
	state := &AppState{Store: fs, Redis: rdb}
	state.Billing = services.NewBillingLifecycleService(fs, services.NewQueueService(rdb), nil, "https://panel.example.com", 48*time.Hour, true)
	h := &BillingHandler{state: state}

	req := httptest.NewRequest("PATCH", "/api/admin/users/u1/billing", strings.NewReader(`{"status":"active"}`))
	req = mux.SetURLVars(req, map[string]string{"id": "u1"})
	ctx := context.WithValue(req.Context(), "username", "admin")
	ctx = context.WithValue(ctx, "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", "admin-1")
	h.SetBillingStatus(httptest.NewRecorder(), req.WithContext(ctx))
	if len(fs.holds) != 1 || fs.holds[0] {
		t.Fatalf("holds = %v, want one clear", fs.holds)
	}
}
