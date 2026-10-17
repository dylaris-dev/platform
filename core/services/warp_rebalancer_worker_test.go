package services

import (
	"context"
	"slices"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/store"
)

// fakeRebalancerStore embeds the full store.Store interface (as a nil value) so
// it satisfies WarpRebalancer's store field, and overrides only the one method
// apply actually calls. Every other store.Store method would panic if invoked
// on a nil embedded interface, which is fine: apply never reaches them.
type fakeRebalancerStore struct {
	store.Store
	assignedCalls []struct{ pubkey, leader string }
}

func newFakeRebalancerStore() *fakeRebalancerStore {
	return &fakeRebalancerStore{}
}

func (f *fakeRebalancerStore) GetWarpPeerByPubkey(pubkey string) (*store.WarpPeer, error) {
	return &store.WarpPeer{Pubkey: pubkey, WGIP: "10.0.99.7"}, nil
}

func (f *fakeRebalancerStore) SetWarpPeerAssignedLeader(pubkey, leaderID string) error {
	f.assignedCalls = append(f.assignedCalls, struct{ pubkey, leader string }{pubkey, leaderID})
	return nil
}

func TestWarpRebalancerApply(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	moves := []warpMove{{Pubkey: "p1", From: "L-a", To: "L-b", TxBps: 100}}

	t.Run("dry-run writes a decision but no assignment and no lastmove", func(t *testing.T) {
		fs := newFakeRebalancerStore() // records SetWarpPeerAssignedLeader calls
		w := NewWarpRebalancer(fs, rc, nil)
		w.apply(ctx, "dry-run", 10, moves)
		if len(fs.assignedCalls) != 0 {
			t.Fatalf("dry-run must not write assignments, got %v", fs.assignedCalls)
		}
		if ex, _ := rc.Exists(ctx, "dylaris:warp:rebalance:lastmove:p1").Result(); ex != 0 {
			t.Fatalf("dry-run must not set lastmove")
		}
		if n, _ := rc.LLen(ctx, "dylaris:warp:rebalance:decisions").Result(); n == 0 {
			t.Fatalf("dry-run must record a decision")
		}
	})

	t.Run("armed writes assignment + lastmove + decision", func(t *testing.T) {
		mr.FlushAll()
		fs := newFakeRebalancerStore()
		w := NewWarpRebalancer(fs, rc, nil)
		w.apply(ctx, "armed", 10, moves)
		if len(fs.assignedCalls) != 1 || fs.assignedCalls[0].pubkey != "p1" || fs.assignedCalls[0].leader != "L-b" {
			t.Fatalf("armed must pin p1 -> L-b, got %v", fs.assignedCalls)
		}
		if ex, _ := rc.Exists(ctx, "dylaris:warp:rebalance:lastmove:p1").Result(); ex != 1 {
			t.Fatalf("armed must set lastmove")
		}
		if n, _ := rc.LLen(ctx, "dylaris:warp:rebalance:decisions").Result(); n == 0 {
			t.Fatalf("armed must record a decision")
		}
		// The handover: the peer must not stay on both leaders.
		if got := queued(t, mr, "L-b"); !slices.Equal(got, []string{"add_peer:p1"}) {
			t.Fatalf("new home queue = %v", got)
		}
		if got := queued(t, mr, "L-a"); !slices.Equal(got, []string{"remove_peer:p1"}) {
			t.Fatalf("old home queue = %v", got)
		}
	})
}
