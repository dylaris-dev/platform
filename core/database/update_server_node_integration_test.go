package database

import "testing"

// A server deleted during a move is not a cutover. The update matched no row,
// reported success, and the move "finished" for a server that no longer exists.
func TestIntegrationMovingADeletedServerIsAnError(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	if err := st.UpdateServerNode(f.server.ID, f.server.NodeID); err != nil {
		t.Fatalf("an existing server: %v", err)
	}
	if err := st.UpdateServerNode(f.server.ID+1_000_000, f.server.NodeID); err == nil {
		t.Fatal("moving a server that does not exist reported success")
	}
}
