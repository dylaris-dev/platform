package servers

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/placement"
	"dylaris-core/store"
)

// placementKitStore adds the remote kit table to the placement fixture.
type placementKitStore struct {
	*placementFakeStore
	kits []store.RemoteKit
}

func (f *placementKitStore) ListRemoteKits() ([]store.RemoteKit, error) { return f.kits, nil }

// On the managed gateway a node whose link is being replaced is listed but
// never picked; on the platform's own gateway the stored kit rows mean nothing.
func TestPickNodeSkipsANodeWhoseLinkIsBeingReplaced(t *testing.T) {
	// Node 1 comes first and is otherwise as good, so nothing but its link
	// can be what passes it over.
	h := newPlacementHandler(t, []models.Node{ownedNode(1, "draining", nil), ownedNode(2, "new", nil)})
	at := time.Now().Add(time.Hour)
	h.d.Store = &placementKitStore{placementFakeStore: h.d.Store.(*placementFakeStore),
		kits: []store.RemoteKit{{NodeID: 1, LinkID: "link-old", RetireAt: &at}, {NodeID: 2, LinkID: "link-new"}}}

	h.d.GatewayRemote = func() (bool, bool) { return true, true }
	got := h.pickNode(context.Background(), placement.PickNodeRequest{RAMMB: 1024})
	if !got.Success || got.Picked == nil || got.Picked.NodeID != 2 {
		t.Fatalf("remote: picked %+v (%s), want node 2", got.Picked, got.Reason)
	}
	if one := h.pickNode(context.Background(), placement.PickNodeRequest{RAMMB: 1024, NodeID: 1}); one.Success {
		t.Fatalf("remote: the named draining node was accepted: %+v", one.Picked)
	}

	h.d.GatewayRemote = func() (bool, bool) { return false, true }
	if got := h.pickNode(context.Background(), placement.PickNodeRequest{RAMMB: 1024, NodeID: 1}); !got.Success {
		t.Fatalf("local: node 1 refused: %s", got.Reason)
	}
}
