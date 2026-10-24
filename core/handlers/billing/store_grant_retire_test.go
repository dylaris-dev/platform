package billing

import (
	"testing"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/services"
	"dylaris-core/store"
)

// retireStore records which grants were ended, which is the whole assertion:
// the purchase itself is written elsewhere and already covered.
type retireStore struct {
	store.Store
	cleared map[string]bool
}

func (f *retireStore) SetUserManualEntitlementKind(_ string, kind string, expiresAt *time.Time, _ string) error {
	if f.cleared == nil {
		f.cleared = map[string]bool{}
	}
	if expiresAt == nil {
		f.cleared[kind] = true
	}
	return nil
}

func n64(v int64) *int64 { return &v }

// A purchase retires the grant for what was bought, and nothing else.
//
// Leaving it behind is not cosmetic: the account reads as "plan and grant"
// forever, the admin screen shows a deadline that decides nothing, and if the
// subscription is later cancelled the stale grant quietly keeps them entitled.
func TestPurchaseRetiresTheGrantItCovers(t *testing.T) {
	tests := []struct {
		name                string
		nodes               *int64
		setNodes            bool
		links               *int64
		setLinks            bool
		wantByon, wantRoute bool
	}{
		{"buying nodes retires the BYON grant", n64(2), true, nil, false, true, false},
		{"buying links retires the route-only grant", nil, false, n64(1), true, false, true},
		{"buying both retires both", n64(1), true, n64(1), true, true, true},
		// A cancellation pushes zero. Clearing on that would take away a grant
		// an admin had given separately, which is the opposite of what a lapsed
		// subscription should do.
		{"a cancellation retires nothing", n64(0), true, n64(0), true, false, false},
		// "The store did not mention this" is not a purchase either.
		{"an unmentioned kind is left alone", nil, true, nil, true, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &retireStore{}
			h := &StoreHandler{state: &handlers.AppState{Store: fs}}
			h.retireGrantsCoveredByPurchase("u1", tt.nodes, tt.setNodes, tt.links, tt.setLinks)
			if got := fs.cleared[services.EntitlementByon]; got != tt.wantByon {
				t.Errorf("byon grant retired = %v, want %v", got, tt.wantByon)
			}
			if got := fs.cleared[services.EntitlementRouteOnly]; got != tt.wantRoute {
				t.Errorf("route-only grant retired = %v, want %v", got, tt.wantRoute)
			}
		})
	}
}
