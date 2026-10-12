package services

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type ipSyncStore struct {
	store.Store
	writes int
	kinds  map[string]string
}

func (f *ipSyncStore) SetNodeIPs(_ int, _ string, _ []string, kinds map[string]string) error {
	f.writes++
	f.kinds = kinds
	return nil
}

// A node updated to report what its addresses are reports the same addresses,
// so the kinds alone must be enough to write them; an unchanged heartbeat
// writes nothing.
func TestSyncNodeIPsStoresKindsForUnchangedAddresses(t *testing.T) {
	st := &ipSyncStore{}
	s := &DiscoveryService{store: st}
	node := &models.Node{ID: 1, PrivateIPs: []string{"10.10.10.8", "172.17.0.1"}}
	kinds := map[string]string{"10.10.10.8": "lan", "172.17.0.1": "docker"}

	s.syncNodeIPs(node, NodeIPs{Private: node.PrivateIPs, Kinds: kinds})
	if st.writes != 1 || st.kinds["172.17.0.1"] != "docker" {
		t.Fatalf("writes = %d, kinds = %v; want the kinds stored", st.writes, st.kinds)
	}

	node.PrivateIPKinds = kinds
	s.syncNodeIPs(node, NodeIPs{Private: node.PrivateIPs, Kinds: kinds})
	if st.writes != 1 {
		t.Errorf("an unchanged heartbeat wrote again (%d writes)", st.writes)
	}
}
