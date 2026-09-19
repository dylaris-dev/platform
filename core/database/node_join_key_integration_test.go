package database

import (
	"strings"
	"testing"

	"dylaris-core/models"
)

// The owner's admission names the key they read off their own machine's log.
// For a customer's machine every connection arrives from the warp leader's
// address, which every customer in the region shares, and the attempt row is
// rewritten by whoever knocks with the machine's id - so neither the address
// nor "what knocked last" can decide it. Only a key the node signs our nonce
// with can.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationAnOwnersAdmissionIsBoundToTheKeyTheyNamed(t *testing.T) {
	_, st := integrationDB(t)
	tok := uniqueName("keybind_")
	t.Cleanup(func() { st.DeleteNodeJoinAttempt(tok) })

	mine := strings.Repeat("a1", 32)
	theirs := strings.Repeat("b2", 32)

	// Someone else keeps knocking as this machine, from another region.
	if err := st.RecordNodeJoinAttempt(models.NodeJoinAttempt{NodeToken: tok, PeerIP: "10.78.0.1", Reason: "refused", PresentedKey: theirs}); err != nil {
		t.Fatalf("record: %v", err)
	}
	a, err := st.GetNodeJoinAttempt(tok)
	if err != nil || a == nil || a.PresentedKey != theirs {
		t.Fatalf("GetNodeJoinAttempt = (%+v, %v), want the presented key", a, err)
	}

	// The owner admits the prefix their node logs. What is knocking does not
	// get in, from any address.
	if ok, err := st.ApproveNodeJoinAttemptForKey(tok, mine[:16], "owner"); err != nil || !ok {
		t.Fatalf("the owner's admission did not arm (ok=%v err=%v)", ok, err)
	}
	if ok, err := st.ConsumeNodeJoinApproval(tok, "10.78.0.1", theirs); err != nil || ok {
		t.Errorf("a key the owner never named was admitted (ok=%v err=%v)", ok, err)
	}
	// And a later knock does not disturb the admission.
	if err := st.RecordNodeJoinAttempt(models.NodeJoinAttempt{NodeToken: tok, PeerIP: "10.78.0.1", Reason: "refused", PresentedKey: theirs}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// The owner's machine, from its own region's leader.
	if ok, err := st.ConsumeNodeJoinApproval(tok, "10.77.0.1", mine); err != nil || !ok {
		t.Errorf("the owner's key was not let in (ok=%v err=%v)", ok, err)
	}
	if ok, err := st.ConsumeNodeJoinApproval(tok, "10.77.0.1", mine); err != nil || ok {
		t.Errorf("the admission was consumed twice (ok=%v err=%v)", ok, err)
	}

	// Too short to be a key is not an admission.
	if ok, err := st.ApproveNodeJoinAttemptForKey(tok, "a1a1", "owner"); err != nil || ok {
		t.Errorf("a four-digit fingerprint armed an admission (ok=%v err=%v)", ok, err)
	}
	// An operator's admission stays bound to the address it was granted for.
	if ok, err := st.ArmNodeJoinApproval(tok, "203.0.113.7", "admin"); err != nil || !ok {
		t.Fatalf("arm: ok=%v err=%v", ok, err)
	}
	if ok, err := st.ConsumeNodeJoinApproval(tok, "198.51.100.1", mine); err != nil || ok {
		t.Errorf("an address-bound admission admitted another address (ok=%v err=%v)", ok, err)
	}
}
