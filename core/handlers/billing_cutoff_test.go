package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type cutoffBillingStore struct {
	store.Store
	billing *store.UserBilling
}

func (f *cutoffBillingStore) GetUserBilling(string) (*store.UserBilling, error) {
	return f.billing, nil
}

// An over-limit tenant past the grace is refused wherever a suspended one is:
// the hourly pass stopped their servers and every gate let them start again
// until the next one. Each cutoff is told its own way out.
func TestAnOverLimitOwnerIsRefusedLikeASuspendedOne(t *testing.T) {
	long := time.Now().Add(-services.OverLimitGrace - time.Hour)
	recent := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		billing store.UserBilling
		admin   bool
		want    string
	}{
		{"active", store.UserBilling{Status: "active"}, false, ""},
		{"suspended", store.UserBilling{Status: "suspended"}, false, suspendedMessage},
		{"over the limit past the grace", store.UserBilling{Status: "active", OverLimitSince: &long}, false, overLimitMessage},
		{"over the limit within the grace", store.UserBilling{Status: "active", OverLimitSince: &recent}, false, ""},
		{"an operator in the panel", store.UserBilling{Status: "active", OverLimitSince: &long}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.billing
			state := &AppState{Store: &cutoffBillingStore{billing: &b}, SuspendGrace: 48 * time.Hour}
			r := httptest.NewRequest("POST", "/", nil)
			r = r.WithContext(context.WithValue(r.Context(), "isAdmin", tc.admin))
			if got := ownerRefusal(state, r, "u1"); got != tc.want {
				t.Fatalf("refusal %q, want %q", got, tc.want)
			}
		})
	}
}

type suspendedRouteStore struct {
	*linkRouteFakeStore
	status string
}

func (f *suspendedRouteStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID, Status: f.status}, nil
}

// A cut-off owner kept claiming addresses on our domains, which nobody else
// could take while they held them. Refused before anything is queued.
func TestASuspendedOwnerCannotClaimAnAddress(t *testing.T) {
	for _, tc := range []struct {
		status     string
		wantStatus int
	}{{"suspended", http.StatusForbidden}, {"active", http.StatusCreated}} {
		t.Run(tc.status, func(t *testing.T) {
			rdb := newLinkRouteRedis(t)
			fs := &suspendedRouteStore{status: tc.status, linkRouteFakeStore: &linkRouteFakeStore{settings: map[string]string{
				services.HosterDomainsSettingKey: `[{"domain":"example.com","validation":"dns"}]`,
			}, servers: map[string][]models.Server{linkRouteUserID: {{ID: 1, UUID: "srv-1"}}}}}
			gw := &linkRouteFakeGateway{}
			h := &GatewayHandler{state: &AppState{Store: fs, Gateway: gw, Redis: rdb, FeatureFlags: services.NewFeatureFlags(routeOnlyOnFlags{})}}
			rec := httptest.NewRecorder()
			h.CreateServerRoute(rec, serverRouteReq(linkRouteUserID, false, map[string]interface{}{
				"domain": "new.example.com", "targetPort": 25565,
			}))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if created := len(gw.createRouteServerCalls) > 0; created != (tc.wantStatus == http.StatusCreated) {
				t.Fatalf("created = %v", created)
			}
		})
	}
}

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
			state := &AppState{Store: fs, Redis: rdb}
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
