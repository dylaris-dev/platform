package database

import (
	"maps"
	"testing"
)

// The kind of each private address survives the round trip, and a node that
// does not classify reads back as "unknown" (nil), not as "none of them is
// anything" ({}), so the panel can fall back to the plain list.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationNodeIPKindsRoundTrip(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	kinds := map[string]string{"10.10.10.8": "lan", "172.17.0.1": "docker"}
	if err := st.SetNodeIPs(f.node.ID, "", []string{"10.10.10.8", "172.17.0.1"}, kinds); err != nil {
		t.Fatalf("SetNodeIPs: %v", err)
	}
	n, err := st.GetNodeByID(f.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(n.PrivateIPKinds, kinds) {
		t.Errorf("kinds = %v, want %v", n.PrivateIPKinds, kinds)
	}

	if err := st.SetNodeIPs(f.node.ID, "", []string{"10.10.10.8"}, nil); err != nil {
		t.Fatalf("SetNodeIPs: %v", err)
	}
	if n, err = st.GetNodeByID(f.node.ID); err != nil {
		t.Fatal(err)
	}
	if n.PrivateIPKinds != nil {
		t.Errorf("an unclassified node reads back as %v, want nil", n.PrivateIPKinds)
	}
}
