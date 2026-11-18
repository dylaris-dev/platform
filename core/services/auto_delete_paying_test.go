package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/services/redisacl"
	"dylaris-core/store"
)

// payingFakeStore is autoDeleteFakeStore plus what the warning stage and the
// billing read need.
type payingFakeStore struct {
	autoDeleteFakeStore

	candidates []store.InactiveCandidate
	billing    map[string]*store.UserBilling
	billingErr error

	marked    []string
	cancelled []string
}

func (f *payingFakeStore) ListInactiveCandidates(time.Time) ([]store.InactiveCandidate, error) {
	return f.candidates, nil
}
func (f *payingFakeStore) GetUserBilling(id string) (*store.UserBilling, error) {
	return f.billing[id], f.billingErr
}
func (f *payingFakeStore) MarkUserPendingDeletion(id string, _ time.Time) error {
	f.marked = append(f.marked, id)
	return nil
}
func (f *payingFakeStore) CancelUserDeletion(id string) error {
	f.cancelled = append(f.cancelled, id)
	return nil
}

// A customer who pays for something is using the platform whether or not they
// log into the panel. A route-only customer with no servers and a link that
// authenticates with its own key never does, so after 90 days the inactive
// sweep anonymized them and tore down their link and addresses while the
// subscription was still running.
func TestAutoDeleteSparesAccountsThatPayOrHoldAGrant(t *testing.T) {
	future := time.Now().Add(30 * 24 * time.Hour)
	cases := []struct {
		name    string
		billing *store.UserBilling
		err     error
		spared  bool
	}{
		{"no billing row", nil, nil, false},
		{"route-only purchase", &store.UserBilling{Status: "active", MaxLinks: i64(1)}, nil, true},
		{"BYON purchase past due", &store.UserBilling{Status: "past_due", MaxNodes: i64(2)}, nil, true},
		{"manual grant", &store.UserBilling{Status: "active", ManualRouteExpiresAt: &future}, nil, true},
		{"cancelled purchase", &store.UserBilling{Status: "active", MaxLinks: i64(0), MaxNodes: i64(0)}, nil, false},
		{"suspended", &store.UserBilling{Status: "suspended", MaxLinks: i64(1)}, nil, false},
		// Not knowing is not "free": the sweep removes accounts.
		{"billing unreadable", nil, errors.New("db down"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			fs := &payingFakeStore{
				autoDeleteFakeStore: autoDeleteFakeStore{due: []string{"owner-1"}},
				candidates:          []store.InactiveCandidate{{ID: "owner-1", Username: "u"}},
				billing:             map[string]*store.UserBilling{"owner-1": tc.billing},
				billingErr:          tc.err,
			}
			svc := NewAutoDeleteService(fs, "https://panel.example.test")
			svc.SetLinkACL(&autoDeleteFakeGateway{store: &fs.autoDeleteFakeStore, failFor: map[string]error{}}, rdb, redisacl.NewProvisioner(rdb))
			p := policySnapshot{Enabled: true, InactiveDays: 90, Mode: "anonymize"}

			svc.processWarnings(context.Background(), p)
			svc.processExecutions(context.Background(), p)

			removed := len(fs.anonIDs) > 0
			if tc.spared {
				if removed || len(fs.marked) > 0 {
					t.Errorf("anonymized=%v warned=%v an account that pays", fs.anonIDs, fs.marked)
				}
				// A pending stamp from before must not survive: it would execute
				// with no fresh warning the day they stop paying.
				if tc.err == nil && len(fs.cancelled) != 1 {
					t.Errorf("pending deletion not cancelled: %v", fs.cancelled)
				}
			} else if !removed || len(fs.marked) != 1 {
				t.Errorf("anonymized=%v warned=%v; a free dormant account must still go", fs.anonIDs, fs.marked)
			}
		})
	}
}
