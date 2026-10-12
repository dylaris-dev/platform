package database

import (
	"testing"

	"dylaris-core/store"
)

// A rolled warp key keeps opening the door for an hour, so a warp or link that
// restarts before the machine is redeployed still comes up. After that only the
// new one does, and a revoked key opens nothing whichever secret is presented.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationARolledWarpKeyKeepsItsOldSecretForAnHour(t *testing.T) {
	db, st := integrationDB(t)
	nodeID := uniqueName("node-")
	oldHash, newHash := uniqueName("old_"), uniqueName("new_")
	id, err := st.CreateWarpAPIKey(store.WarpAPIKey{
		Name: "ext", KeyHash: oldHash, Policy: "general", MaxConns: 1, OnNewConn: "kill_old", NodeID: nodeID,
	})
	if err != nil {
		t.Fatalf("CreateWarpAPIKey: %v", err)
	}
	if err := st.RollWarpAPIKeyHash(nodeID, newHash); err != nil {
		t.Fatalf("RollWarpAPIKeyHash: %v", err)
	}
	for _, h := range []string{oldHash, newHash} {
		if k, err := st.GetWarpAPIKeyByHash(h); err != nil || k.ID != id {
			t.Errorf("within the grace, lookup of %s = (%v, %v), want key %d", h, k, err, id)
		}
	}

	if _, err := db.Exec(`UPDATE warp_api_keys SET prev_key_expires_at = NOW() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if k, err := st.GetWarpAPIKeyByHash(oldHash); err == nil {
		t.Errorf("after the grace the old secret still opens key %d", k.ID)
	}
	if k, err := st.GetWarpAPIKeyByHash(newHash); err != nil || k.ID != id {
		t.Errorf("after the grace, new secret = (%v, %v), want key %d", k, err, id)
	}

	// A second roll moves the grace to the secret it replaced.
	newer := uniqueName("newer_")
	if err := st.RollWarpAPIKeyHash(nodeID, newer); err != nil {
		t.Fatalf("second roll: %v", err)
	}
	if _, err := st.GetWarpAPIKeyByHash(newHash); err != nil {
		t.Errorf("second roll: the secret it replaced is refused: %v", err)
	}
	if _, err := st.GetWarpAPIKeyByHash(oldHash); err == nil {
		t.Error("second roll brought the first secret back")
	}
}
