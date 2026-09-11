package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"
)

// The rule these decide: minting the thing you did not buy is refused, and
// refused with a reason the panel can act on.
//
// What they pin is a hole that was open in production. The only guard on the
// mint endpoints was the numeric cap:
//
//	if lim.MaxNodes > 0 { ...refuse when over... }
//
// An account that has bought nothing has no override row, so MaxNodes is 0, so
// the body never runs, so the account is uncapped. A user could register, verify
// their email, and immediately mint node enroll tokens and route-only link kits
// without ever reaching the store. The one value meaning "paid for nothing" was
// the one value that switched the limit off.

// entGateStore serves the two lookups the gate makes.
type entGateStore struct {
	store.Store
	settings map[string]string
	billing  *store.UserBilling
}

func (f *entGateStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
func (f *entGateStore) GetUserBilling(string) (*store.UserBilling, error) {
	return f.billing, nil
}
func (f *entGateStore) CountNodesByOwner(string) (int, error)            { return 0, nil }
func (f *entGateStore) CountPendingNodeEnrollTokens(string) (int, error) { return 0, nil }

// Both pending kinds, because services.NodeSlotsUsed asks for both: a tenant can
// reach a node through an enroll token OR a warp key, and a fake that answers
// only the one this file is about would let the mint gate be tested against a
// number the production gate never sees.
func (f *entGateStore) CountNodeWarpKeysByOwner(string) (int, error) { return 0, nil }
func (f *entGateStore) CreateNodeEnrollToken(string, string, string, *time.Time, string) error {
	return nil
}

// newEntGateState builds a BYON-on state. storeEnabled is what separates a
// hosted install (entitlement required) from a self-host one (everything
// allowed), and getting it wrong in either direction is the whole risk here.
func newEntGateState(billing *store.UserBilling, storeEnabled bool) *AppState {
	fs := &entGateStore{
		settings: map[string]string{"feature_byon_enabled": "true"},
		billing:  billing,
	}
	return &AppState{
		Store:        fs,
		FeatureFlags: services.NewFeatureFlags(fs),
		StoreEnabled: storeEnabled,
		// Deliberately blank: an unreachable storefront must not turn the refusal
		// into a "you are not connected" claim we cannot support. See
		// storeLinkedBestEffort.
		StoreURL: "",
	}
}

func mintEnrollToken(state *AppState, userID string) *httptest.ResponseRecorder {
	h := NewNodeEnrollHandler(state)
	rec := httptest.NewRecorder()
	h.MintToken(rec, nodeEnrollMintReq(userID, map[string]interface{}{}))
	return rec
}

func decodeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out.Code
}

func TestMintToken_RefusedWithoutEntitlement(t *testing.T) {
	cases := []struct {
		name     string
		billing  *store.UserBilling
		wantCode string
	}{
		{
			// The exact account the hole was found with: registered, verified,
			// never went near the store.
			name:     "a fresh account has no billing row",
			billing:  nil,
			wantCode: DenyNoEntitlement,
		},
		{
			// What a cancellation pushes. Reads as unlimited to a bare cap check.
			name:     "an explicit zero is not unlimited",
			billing:  &store.UserBilling{Status: "active", MaxNodes: ptrI64(0)},
			wantCode: DenyNoEntitlement,
		},
		{
			name:     "route-only does not buy a node",
			billing:  &store.UserBilling{Status: "active", MaxLinks: ptrI64(1)},
			wantCode: DenyNoEntitlement,
		},
		{
			name:     "suspended is named as suspended, not as unpurchased",
			billing:  &store.UserBilling{Status: "suspended", MaxNodes: ptrI64(1)},
			wantCode: DenySuspended,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := mintEnrollToken(newEntGateState(tc.billing, true), "u1")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if got := decodeCode(t, rec); got != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", got, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// The other direction, which matters just as much: the gate must not lock out
// people who did buy, and must not exist at all on a self-host install.
func TestMintToken_AllowedWithEntitlement(t *testing.T) {
	cases := []struct {
		name         string
		billing      *store.UserBilling
		storeEnabled bool
	}{
		{"a purchased node", &store.UserBilling{Status: "active", MaxNodes: ptrI64(1)}, true},
		{"past_due keeps working through the grace window", &store.UserBilling{Status: "past_due", MaxNodes: ptrI64(1)}, true},
		{"an admin grant with no purchase", &store.UserBilling{
			Status:                     "active",
			ManualEntitlement:          services.EntitlementByon,
			ManualEntitlementExpiresAt: ptrT(time.Now().Add(24 * time.Hour)),
		}, true},
		{"self-host: no store, no billing row, allowed", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := mintEnrollToken(newEntGateState(tc.billing, tc.storeEnabled), "u1")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func ptrI64(i int64) *int64       { return &i }
func ptrT(t time.Time) *time.Time { return &t }

// An administrator is not a customer of the platform they run.
//
// Every other admin gate in this codebase reads it that way (canManageNode opens
// with it) and this one did not, so on a hosted install with the store live the
// owner could not mint an enroll token for their own machine without selling
// themselves a subscription first. The billing row here is the worst case a
// customer could have - suspended, nothing bought - and none of it describes an
// operator.
func TestMintToken_AdminIsNotSubjectToTheGate(t *testing.T) {
	state := newEntGateState(&store.UserBilling{Status: "suspended"}, true)
	h := NewNodeEnrollHandler(state)

	req := nodeEnrollMintReq("admin-1", map[string]interface{}{})
	req = req.WithContext(context.WithValue(req.Context(), "isAdmin", true))

	rec := httptest.NewRecorder()
	h.MintToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// The mirror, so this is about who is asking and not a way past the gate:
	// the same row without the admin flag is still refused.
	rec = httptest.NewRecorder()
	h.MintToken(rec, nodeEnrollMintReq("u1", map[string]interface{}{}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an ordinary account: %s", rec.Code, rec.Body.String())
	}
}
