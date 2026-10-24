package billing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// trafficNotifyFakeStore is a settable traffic_limits table plus the two calls
// the notification makes.
type trafficNotifyFakeStore struct {
	store.Store
	rows     map[string]*models.TrafficLimit // "scope|region|kind"
	tenants  []store.UserBilling
	notified []models.Notification
}

func (f *trafficNotifyFakeStore) key(scope, region, kind string) string {
	return scope + "|" + region + "|" + kind
}

func (f *trafficNotifyFakeStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	return f.rows[f.key(scope, region, kind)], nil
}

func (f *trafficNotifyFakeStore) SetTrafficLimit(scope, region, kind string, included, maxPurchase *int64) error {
	f.rows[f.key(scope, region, kind)] = &models.TrafficLimit{
		Scope: scope, Region: region, Kind: kind,
		IncludedGB: included, MaxPurchaseGB: maxPurchase,
	}
	return nil
}

func (f *trafficNotifyFakeStore) DeleteTrafficLimit(scope, region, kind string) error {
	delete(f.rows, f.key(scope, region, kind))
	return nil
}

func (f *trafficNotifyFakeStore) ListUserBilling() ([]store.UserBilling, error) {
	return f.tenants, nil
}

func (f *trafficNotifyFakeStore) InsertNotification(n *models.Notification) (int64, error) {
	f.notified = append(f.notified, *n)
	return int64(len(f.notified)), nil
}

func trafficLimitPut(t *testing.T, h *TrafficLimitHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.SetTrafficLimit(rec, httptest.NewRequest(http.MethodPut, "/api/traffic-limits", bytes.NewReader(b)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec
}

func consenting(id string, units int64) store.UserBilling {
	return store.UserBilling{UserID: id, MaxNodes: &units, TrafficBillingEnabled: true}
}

// Raising the purchase cap raises what a consenting tenant can be charged, so
// they are told - and the message carries the number they were on and the number
// they are on now.
//
// Driven through the handler rather than the service because the before/after
// pair is assembled HERE, around a write that has already happened. A comparison
// made after the row was replaced would see two identical values and go quiet.
func TestSetTrafficLimitNotifiesWhenThePurchaseCapMoves(t *testing.T) {
	st := &trafficNotifyFakeStore{
		rows: map[string]*models.TrafficLimit{
			"user_default|eu-central|edge": {IncludedGB: services.LimitPtr(1000), MaxPurchaseGB: services.LimitPtr(2000)},
		},
		tenants: []store.UserBilling{consenting("u1", 1)},
	}
	h := &TrafficLimitHandler{state: &handlers.AppState{Store: st}}

	trafficLimitPut(t, h, map[string]any{
		"scope": "user_default", "region": "eu-central", "kind": "edge",
		"includedMode": "custom", "includedGb": 1000,
		"purchaseMode": "custom", "purchaseGb": 5000,
	})

	if len(st.notified) != 1 {
		t.Fatalf("wrote %d notifications, want 1", len(st.notified))
	}
	body := st.notified[0].Body
	if !strings.Contains(body, "2000 GB") || !strings.Contains(body, "5000 GB") {
		t.Errorf("body should state both ceilings: %q", body)
	}
}

// Changing only the INCLUDED allowance tells nobody: what a consenting tenant may
// book on top has not moved, and the notification is about that ceiling.
func TestSetTrafficLimitQuietWhenOnlyIncludedChanges(t *testing.T) {
	st := &trafficNotifyFakeStore{
		rows: map[string]*models.TrafficLimit{
			"user_default|eu-central|edge": {IncludedGB: services.LimitPtr(1000), MaxPurchaseGB: services.LimitPtr(2000)},
		},
		tenants: []store.UserBilling{consenting("u1", 1)},
	}
	h := &TrafficLimitHandler{state: &handlers.AppState{Store: st}}

	trafficLimitPut(t, h, map[string]any{
		"scope": "user_default", "region": "eu-central", "kind": "edge",
		"includedMode": "custom", "includedGb": 4000,
		"purchaseMode": "custom", "purchaseGb": 2000,
	})

	if len(st.notified) != 0 {
		t.Errorf("wrote %d notifications for an unchanged purchase cap: %+v", len(st.notified), st.notified)
	}
}

// Clearing a row is a change even though nobody typed a number: the scope stops
// answering, so the tenants it covered fall through to whatever is left.
func TestClearingATrafficLimitNotifies(t *testing.T) {
	st := &trafficNotifyFakeStore{
		rows: map[string]*models.TrafficLimit{
			"user_default|eu-central|edge": {IncludedGB: services.LimitPtr(1000), MaxPurchaseGB: services.LimitPtr(2000)},
		},
		tenants: []store.UserBilling{consenting("u1", 1)},
	}
	h := &TrafficLimitHandler{state: &handlers.AppState{Store: st}}

	trafficLimitPut(t, h, map[string]any{
		"scope": "user_default", "region": "eu-central", "kind": "edge",
		"includedMode": "default", "purchaseMode": "default",
	})

	if len(st.rows) != 0 {
		t.Fatalf("the row survived the clear: %+v", st.rows)
	}
	if len(st.notified) != 1 {
		t.Fatalf("wrote %d notifications, want 1", len(st.notified))
	}
	if !strings.Contains(st.notified[0].Body, "no limit") {
		t.Errorf("a cleared cap should read as no limit: %q", st.notified[0].Body)
	}
}
