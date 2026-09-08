package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// tenancyFlagStore is a minimal settingsReader fake (services.FeatureFlags's
// unexported dependency interface - GetSetting(key string) (string, error))
// used to construct a real *services.FeatureFlags with the BYON flag pinned
// on or off, without needing a full store.Store fake.
type tenancyFlagStore struct {
	val string
}

func (f tenancyFlagStore) GetSetting(string) (string, error) { return f.val, nil }

func tenancyFeatureFlags(byonEnabled bool) *services.FeatureFlags {
	v := "false"
	if byonEnabled {
		v = "true"
	}
	return services.NewFeatureFlags(tenancyFlagStore{val: v})
}

func tenancyRequest(userID string, isAdmin bool) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(r.Context(), "isAdmin", isAdmin)
	if userID != "" {
		ctx = context.WithValue(ctx, "userID", userID)
	}
	return r.WithContext(ctx)
}

func strPtrTenancy(s string) *string { return &s }

func TestByonActive(t *testing.T) {
	cases := []struct {
		name  string
		state *AppState
		want  bool
	}{
		{"nil state is inactive", nil, false},
		{"nil FeatureFlags is inactive", &AppState{}, false},
		{"flag off", &AppState{FeatureFlags: tenancyFeatureFlags(false)}, false},
		{"flag on", &AppState{FeatureFlags: tenancyFeatureFlags(true)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := tenancyRequest("u1", false)
			if got := byonActive(c.state, r); got != c.want {
				t.Errorf("byonActive() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestByonCallerID(t *testing.T) {
	cases := []struct {
		name   string
		userID string
		want   string
	}{
		{"userID present in context", "u1", "u1"},
		{"no userID in context returns empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := tenancyRequest(c.userID, false)
			if got := byonCallerID(r); got != c.want {
				t.Errorf("byonCallerID() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestCanManageNode pins the node-management authz gate (tenancy.go): admin
// always passes; otherwise BYON must be active AND the caller must be the
// node's owner. A shared/platform node (OwnerID == nil) is never manageable
// by a non-admin, even with BYON active and even if the caller "would" be
// the owner of some other node.
func TestCanManageNode(t *testing.T) {
	const owner = "owner-1"
	const other = "other-1"

	cases := []struct {
		name    string
		isAdmin bool
		userID  string
		byon    bool
		node    *models.Node
		want    bool
	}{
		{"admin bypass regardless of BYON state or ownership", true, other, false, &models.Node{OwnerID: strPtrTenancy(owner)}, true},
		{"admin bypass with nil node", true, other, false, nil, true},
		{"BYON active, caller is the owner", false, owner, true, &models.Node{OwnerID: strPtrTenancy(owner)}, true},
		{"BYON active, caller is not the owner", false, other, true, &models.Node{OwnerID: strPtrTenancy(owner)}, false},
		{"BYON inactive denies even the real owner", false, owner, false, &models.Node{OwnerID: strPtrTenancy(owner)}, false},
		{"BYON active but node is shared (OwnerID nil) denies non-admin", false, owner, true, &models.Node{OwnerID: nil}, false},
		{"BYON active but node is nil denies non-admin", false, owner, true, nil, false},
		{"BYON active, empty caller userID denies", false, "", true, &models.Node{OwnerID: strPtrTenancy(owner)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := &AppState{FeatureFlags: tenancyFeatureFlags(c.byon)}
			r := tenancyRequest(c.userID, c.isAdmin)
			if got := canManageNode(state, r, c.node); got != c.want {
				t.Errorf("canManageNode() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCanPlaceOnNode mirrors TestCanManageNode: as of tenancy.go, the two
// functions are byte-identical in logic. Pinning the same matrix here means
// a future intentional divergence between "manage" and "place" is visible in
// the diff instead of being masked by only one function having coverage.
func TestCanPlaceOnNode(t *testing.T) {
	const owner = "owner-1"
	const other = "other-1"

	cases := []struct {
		name    string
		isAdmin bool
		userID  string
		byon    bool
		node    *models.Node
		want    bool
	}{
		{"admin bypass regardless of BYON state or ownership", true, other, false, &models.Node{OwnerID: strPtrTenancy(owner)}, true},
		{"BYON active, caller is the owner", false, owner, true, &models.Node{OwnerID: strPtrTenancy(owner)}, true},
		{"BYON active, caller is not the owner", false, other, true, &models.Node{OwnerID: strPtrTenancy(owner)}, false},
		{"BYON inactive denies even the real owner", false, owner, false, &models.Node{OwnerID: strPtrTenancy(owner)}, false},
		{"platform (shared) node stays operator-only even in BYON mode", false, owner, true, &models.Node{OwnerID: nil}, false},

		// The hole this table did not cover: with BYON ACTIVE the admin bypass
		// above made every tenant machine a placement target for any operator,
		// while auto-placement (PlatformOnly), the pick loop and the rebalance
		// worker all refused to cross that boundary. A customer's own hardware
		// is not the operator's capacity.
		{"admin may NOT place on another tenant's node while BYON is active", true, other, true, &models.Node{OwnerID: strPtrTenancy(owner)}, false},
		{"admin who owns the node may place on it", true, owner, true, &models.Node{OwnerID: strPtrTenancy(owner)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := &AppState{FeatureFlags: tenancyFeatureFlags(c.byon)}
			r := tenancyRequest(c.userID, c.isAdmin)
			if got := canPlaceOnNode(state, r, c.node); got != c.want {
				t.Errorf("canPlaceOnNode() = %v, want %v", got, c.want)
			}
		})
	}
}
