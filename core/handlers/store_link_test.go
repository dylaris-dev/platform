package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// storeLinkFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods LinkVerify/Provision (directly,
// or via BillingLifecycleService) touch are overridden. Any other call would
// panic - these tests never make one.
type storeLinkFakeStore struct {
	store.Store

	users          map[string]*models.User
	getUserByIDErr error

	settings map[string]string

	setUserBillingStatusCalls []storeLinkBillingStatusCall
	setUserBillingStatusErr   error
	// billingStatus is what GetUserBilling reports; empty means active.
	billingStatus string

	setUserPlanCalls []storeLinkSetPlanCall
	setUserPlanErr   error

	entitlementCalls []storeLinkEntitlementCall
	entitlementErr   error

	routeLimitSets    []storeLinkRouteLimitCall
	routeLimitDeletes []string
	routeLimitErr     error

	// Which manual grants a provision call retired. A purchase ends the grant
	// it covers, so this is part of what "activate" does now.
	retiredGrants []string
}

type storeLinkEntitlementCall struct {
	userID   string
	maxNodes *int64
	setNodes bool
	maxLinks *int64
	setLinks bool
}

type storeLinkBillingStatusCall struct {
	userID     string
	status     string
	hasGrace   bool
	hasSuspend bool
}

type storeLinkSetPlanCall struct {
	userID string
	planID int
}

func (f *storeLinkFakeStore) GetUserByID(id string) (*models.User, error) {
	if f.getUserByIDErr != nil {
		return nil, f.getUserByIDErr
	}
	u, ok := f.users[id]
	if !ok {
		return nil, errors.New("user not found")
	}
	return u, nil
}

// GetUserBilling backs EnterPastDue's effectiveSpec lookup. The returned row's
// empty GracePeriod falls through to the platform setting, then the built-in
// default - none of which this test suite needs to pin precisely.
func (f *storeLinkFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	if f.billingStatus != "" {
		now := time.Now()
		return &store.UserBilling{UserID: userID, Status: f.billingStatus, SuspendedAt: &now}, nil
	}
	return &store.UserBilling{UserID: userID, Status: "active"}, nil
}

// A failed payment retry reaching a tenant who is already suspended must not put
// them back into grace: that was every Stripe retry lifting the cutoff.
func TestProvision_PastDueNeverLiftsASuspension(t *testing.T) {
	fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}, billingStatus: "suspended"}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "past_due"}, storeLinkTestKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setUserBillingStatusCalls) != 0 {
		t.Fatalf("a past_due rewrote a suspended tenant: %+v", fs.setUserBillingStatusCalls)
	}
}

func (f *storeLinkFakeStore) SetUserBillingStatus(userID, status string, graceUntil, suspendedAt *time.Time) error {
	f.setUserBillingStatusCalls = append(f.setUserBillingStatusCalls, storeLinkBillingStatusCall{
		userID: userID, status: status, hasGrace: graceUntil != nil, hasSuspend: suspendedAt != nil,
	})
	return f.setUserBillingStatusErr
}

// max is a POINTER, so a recorded call distinguishes "wrote a cap of 0" from
// "wrote no cap at all" - the two the old int collapsed, and the reason a zero
// address allowance used to end up meaning unlimited.
type storeLinkRouteLimitCall struct {
	scope string
	max   *int
}

func (f *storeLinkFakeStore) SetGatewayRouteLimit(scope string, max *int) error {
	f.routeLimitSets = append(f.routeLimitSets, storeLinkRouteLimitCall{scope, max})
	return f.routeLimitErr
}

func (f *storeLinkFakeStore) DeleteGatewayRouteLimit(scope string) error {
	f.routeLimitDeletes = append(f.routeLimitDeletes, scope)
	return f.routeLimitErr
}

func (f *storeLinkFakeStore) SetUserPurchasedEntitlement(userID string, maxNodes *int64, setNodes bool, maxLinks *int64, setLinks bool) error {
	f.entitlementCalls = append(f.entitlementCalls, storeLinkEntitlementCall{userID, maxNodes, setNodes, maxLinks, setLinks})
	return f.entitlementErr
}

// A purchase retires the manual grant for what it covers. Nil expiry is the
// revoke; anything else is not this path and is recorded as nothing.
func (f *storeLinkFakeStore) SetUserManualEntitlementKind(_ string, kind string, expiresAt *time.Time, _ string) error {
	if expiresAt == nil {
		f.retiredGrants = append(f.retiredGrants, kind)
	}
	return nil
}

func (f *storeLinkFakeStore) SetUserPlan(userID string, planID *int) error {
	pid := 0
	if planID != nil {
		pid = *planID
	}
	f.setUserPlanCalls = append(f.setUserPlanCalls, storeLinkSetPlanCall{userID, pid})
	return f.setUserPlanErr
}

func (f *storeLinkFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

func newStoreLinkRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

const storeLinkTestKey = "test-shared-key"

func newStoreLinkHandler(fs *storeLinkFakeStore, rdb *redis.Client, withBilling bool) *StoreHandler {
	state := &AppState{
		Store:          fs,
		Redis:          rdb,
		StoreEnabled:   true,
		StoreSharedKey: storeLinkTestKey,
	}
	if withBilling {
		state.Billing = services.NewBillingLifecycleService(fs, services.NewQueueService(rdb), nil, "https://panel.example.com", 48*time.Hour, true)
	}
	return &StoreHandler{state: state}
}

func storeLinkPost(path string, body map[string]interface{}, storeKey string) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	if storeKey != "" {
		r.Header.Set("X-Store-Key", storeKey)
	}
	return r.WithContext(context.Background())
}

// --- requireStoreKey gate (shared by both LinkVerify and Provision) ---

func TestStoreHandler_RequireStoreKeyGate(t *testing.T) {
	t.Run("store integration disabled returns 404", func(t *testing.T) {
		fs := &storeLinkFakeStore{}
		h := &StoreHandler{state: &AppState{Store: fs, StoreEnabled: false}}
		rec := httptest.NewRecorder()
		h.LinkVerify(rec, storeLinkPost("/api/store/link/verify", map[string]interface{}{"token": "x"}, "anything"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing or wrong store key returns 401", func(t *testing.T) {
		fs := &storeLinkFakeStore{}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), false)
		rec := httptest.NewRecorder()
		h.LinkVerify(rec, storeLinkPost("/api/store/link/verify", map[string]interface{}{"token": "x"}, "wrong-key"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- LinkVerify: single-use token consume via Redis.GetDel ---

func TestLinkVerify_ValidTokenConsumedOnce(t *testing.T) {
	fs := &storeLinkFakeStore{}
	rdb := newStoreLinkRedis(t)
	h := newStoreLinkHandler(fs, rdb, false)

	if err := rdb.Set(context.Background(), storeLinkTokenPrefix+"tok1", "uuid-1\nuser@example.com\nsomeuser", storeLinkTokenTTL).Err(); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	rec := httptest.NewRecorder()
	h.LinkVerify(rec, storeLinkPost("/api/store/link/verify", map[string]interface{}{"token": "tok1"}, storeLinkTestKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success  bool   `json:"success"`
		UUID     string `json:"uuid"`
		Email    string `json:"email"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.UUID != "uuid-1" || resp.Email != "user@example.com" || resp.Username != "someuser" {
		t.Fatalf("response = %+v", resp)
	}

	// Second consume attempt: GetDel already removed the key, so this must fail.
	rec2 := httptest.NewRecorder()
	h.LinkVerify(rec2, storeLinkPost("/api/store/link/verify", map[string]interface{}{"token": "tok1"}, storeLinkTestKey))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("second use status = %d, want 401 (single-use): %s", rec2.Code, rec2.Body.String())
	}
}

func TestLinkVerify_TolerantOfAbsentUsername(t *testing.T) {
	fs := &storeLinkFakeStore{}
	rdb := newStoreLinkRedis(t)
	h := newStoreLinkHandler(fs, rdb, false)
	if err := rdb.Set(context.Background(), storeLinkTokenPrefix+"tok2", "uuid-2\nuser2@example.com", storeLinkTokenTTL).Err(); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	rec := httptest.NewRecorder()
	h.LinkVerify(rec, storeLinkPost("/api/store/link/verify", map[string]interface{}{"token": "tok2"}, storeLinkTestKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Username != "" {
		t.Fatalf("username = %q, want empty for an old 2-part token value", resp.Username)
	}
}

func TestLinkVerify_MalformedPayload(t *testing.T) {
	fs := &storeLinkFakeStore{}
	rdb := newStoreLinkRedis(t)
	h := newStoreLinkHandler(fs, rdb, false)
	// A value with no newline at all: SplitN yields a single-element slice,
	// len(parts) < 2, so the handler reports it as malformed (500).
	if err := rdb.Set(context.Background(), storeLinkTokenPrefix+"tok3", "no-newline-value", storeLinkTokenTTL).Err(); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	rec := httptest.NewRecorder()
	h.LinkVerify(rec, storeLinkPost("/api/store/link/verify", map[string]interface{}{"token": "tok3"}, storeLinkTestKey))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

func TestLinkVerify_MissingTokenField(t *testing.T) {
	fs := &storeLinkFakeStore{}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), false)
	rec := httptest.NewRecorder()
	h.LinkVerify(rec, storeLinkPost("/api/store/link/verify", map[string]interface{}{}, storeLinkTestKey))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestLinkVerify_UnknownOrExpiredToken(t *testing.T) {
	fs := &storeLinkFakeStore{}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), false)
	rec := httptest.NewRecorder()
	h.LinkVerify(rec, storeLinkPost("/api/store/link/verify", map[string]interface{}{"token": "never-existed"}, storeLinkTestKey))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

// --- Provision: action switch driving h.state.Billing ---

func TestProvision_BillingNotAvailable(t *testing.T) {
	fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), false) // withBilling=false -> h.state.Billing stays nil
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "activate"}, storeLinkTestKey))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func TestProvision_UserNotFound(t *testing.T) {
	fs := &storeLinkFakeStore{}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "ghost", "action": "activate"}, storeLinkTestKey))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestProvision_InvalidRequest_MissingUUID(t *testing.T) {
	fs := &storeLinkFakeStore{}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"action": "activate"}, storeLinkTestKey))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestProvision_UnknownAction(t *testing.T) {
	fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
	h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
	rec := httptest.NewRecorder()
	h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "cancel-everything"}, storeLinkTestKey))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setUserBillingStatusCalls) != 0 {
		t.Fatalf("expected no billing status writes for an unknown action, got %+v", fs.setUserBillingStatusCalls)
	}
}

// TestProvision_ActionRouting pins each action to its exact Billing call, via
// the SetUserBillingStatus writes the real BillingLifecycleService makes
// underneath Reactivate/EnterPastDue/Suspend (store.go:229-254).
func TestProvision_ActionRouting(t *testing.T) {
	t.Run("activate calls Reactivate -> status=active, no grace/suspend timestamps", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "activate"}, storeLinkTestKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setUserBillingStatusCalls) != 1 {
			t.Fatalf("setUserBillingStatusCalls = %+v, want exactly 1", fs.setUserBillingStatusCalls)
		}
		got := fs.setUserBillingStatusCalls[0]
		if got.userID != "u1" || got.status != "active" || got.hasGrace || got.hasSuspend {
			t.Fatalf("call = %+v, want {u1 active false false}", got)
		}
		if len(fs.setUserPlanCalls) != 0 {
			t.Fatalf("expected no SetUserPlan call without a planId, got %+v", fs.setUserPlanCalls)
		}
	})

	// planId is accepted and ignored. Plan tiers are gone, so assigning one
	// wrote a column no resolver reads while answering the store with success -
	// a path that reported work it did not do. An older store that still sends
	// the field must keep working, and what it actually needed all along is the
	// entitlement, which travels as maxNodes/maxLinks.
	t.Run("activate with a planId no longer assigns a plan and still succeeds", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "activate", "planId": 7}, storeLinkTestKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setUserPlanCalls) != 0 {
			t.Fatalf("still assigned a plan: %+v", fs.setUserPlanCalls)
		}
	})

	t.Run("past_due calls EnterPastDue -> status=past_due with a grace deadline", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "past_due"}, storeLinkTestKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setUserBillingStatusCalls) != 1 {
			t.Fatalf("setUserBillingStatusCalls = %+v, want exactly 1", fs.setUserBillingStatusCalls)
		}
		got := fs.setUserBillingStatusCalls[0]
		if got.status != "past_due" || !got.hasGrace || got.hasSuspend {
			t.Fatalf("call = %+v, want status=past_due with a grace deadline and no suspend timestamp", got)
		}
	})

	t.Run("suspend calls Suspend -> status=suspended with a suspended_at timestamp", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "suspend"}, storeLinkTestKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.setUserBillingStatusCalls) != 1 {
			t.Fatalf("setUserBillingStatusCalls = %+v, want exactly 1", fs.setUserBillingStatusCalls)
		}
		got := fs.setUserBillingStatusCalls[0]
		if got.status != "suspended" || got.hasGrace || !got.hasSuspend {
			t.Fatalf("call = %+v, want status=suspended with a suspended_at timestamp and no grace", got)
		}
	})

	t.Run("suspend returns 500 when the store write fails", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{"u1": {ID: "u1"}}, setUserBillingStatusErr: errors.New("db down")}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{"uuid": "u1", "action": "suspend"}, storeLinkTestKey))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
	})
}
