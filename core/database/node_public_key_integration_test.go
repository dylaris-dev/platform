package database

import (
	"strings"
	"testing"

	"dylaris-core/models"
)

// Reset pairing is the compromise response, so it disarms an admission armed
// before it - here the one Roll key arms, fifteen minutes from the node's last
// address. Otherwise whoever sits at that address re-pairs with a fresh key right
// after the Reset and is handed the secret the Reset has just made. The operator's
// own Admit afterwards is how a reset node comes back, and must still work.
func TestIntegrationResetPairingDisarmsAnEarlierAdmission(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	const addr = "203.0.113.7"

	if armed, err := st.ArmNodeJoinApproval(f.node.Token, addr, ""); err != nil || !armed {
		t.Fatalf("Roll key's admission = (%v, %v), want armed", armed, err)
	}
	if err := st.ResetNodeLogin(f.node.ID); err != nil {
		t.Fatalf("ResetNodeLogin: %v", err)
	}
	// The re-pair through the old admission finds nothing to consume. Core then
	// refuses the connect and records it, which is what puts the node under
	// Connection attempts.
	if ok, err := st.ConsumeNodeJoinApproval(f.node.Token, addr); err != nil || ok {
		t.Fatalf("the admission armed before the Reset was consumed after it (%v, %v)", ok, err)
	}
	if err := st.RecordNodeJoinAttempt(models.NodeJoinAttempt{
		NodeToken: f.node.Token, PeerIP: addr, Reason: "waiting to be admitted",
	}); err != nil {
		t.Fatalf("RecordNodeJoinAttempt: %v", err)
	}
	attempts, err := st.ListNodeJoinAttempts()
	if err != nil {
		t.Fatalf("ListNodeJoinAttempts: %v", err)
	}
	listed := false
	for _, a := range attempts {
		listed = listed || a.NodeToken == f.node.Token
	}
	if !listed {
		t.Error("the refused re-pair is not listed under Connection attempts")
	}

	// Admit after the Reset: armed again, and consumed exactly once.
	if armed, err := st.ApproveNodeJoinAttempt(f.node.Token, ""); err != nil || !armed {
		t.Fatalf("Admit after the Reset = (%v, %v), want armed", armed, err)
	}
	if ok, err := st.ConsumeNodeJoinApproval(f.node.Token, addr); err != nil || !ok {
		t.Errorf("the Admit given after the Reset did not admit the node (%v, %v)", ok, err)
	}
}

// Reset pairing's write is one statement: the key moved aside AND the secret
// cleared, so no failure can leave one without the other.
func TestIntegrationResetNodeLoginIsOneWrite(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	id := f.node.ID

	key := strings.Repeat("aa", 32)
	if won, err := st.SetNodePublicKeyIfUnchanged(id, "", "", key); err != nil || !won {
		t.Fatalf("register key = (%v, %v)", won, err)
	}
	if err := st.SetNodeSecretEnc(id, "sealed-secret-placeholder"); err != nil {
		t.Fatalf("SetNodeSecretEnc: %v", err)
	}
	for round := 1; round <= 2; round++ {
		if err := st.ResetNodeLogin(id); err != nil {
			t.Fatalf("ResetNodeLogin round %d: %v", round, err)
		}
		// The second round is a reset before the node re-paired: the rejected key
		// stays instead of being blanked.
		if k, r, err := st.GetNodePublicKeys(id); err != nil || k != "" || r != key {
			t.Errorf("round %d keys = (%q, %q, %v), want ('', key)", round, k, r, err)
		}
		if enc, err := st.GetNodeSecretEnc(id); err != nil || enc != "" {
			t.Errorf("round %d secret = (%q, %v), want cleared", round, enc, err)
		}
	}
	if err := st.ResetNodeLogin(-1); err == nil {
		t.Error("resetting a row that does not exist reported success")
	}
}

// The writes the key login and Reset pairing, Roll key and an approval depend
// on, against a real Postgres. RejectNodePublicKey's SET reads the row as it WAS
// and its RETURNING the row as it IS, and SetNodePublicKeyIfUnchanged must refuse
// a write read before a concurrent one landed; a fake cannot tell whether
// either holds.
func TestIntegrationNodePublicKeyRejection(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	id := f.node.ID

	keys := func() (string, string) {
		t.Helper()
		k, r, err := st.GetNodePublicKeys(id)
		if err != nil {
			t.Fatalf("GetNodePublicKeys: %v", err)
		}
		return k, r
	}
	reject := func() bool {
		t.Helper()
		keyNode, err := st.RejectNodePublicKey(id)
		if err != nil {
			t.Fatalf("RejectNodePublicKey: %v", err)
		}
		return keyNode
	}
	cas := func(prevKey, prevRejected, next string) bool {
		t.Helper()
		won, err := st.SetNodePublicKeyIfUnchanged(id, prevKey, prevRejected, next)
		if err != nil {
			t.Fatalf("SetNodePublicKeyIfUnchanged: %v", err)
		}
		return won
	}

	if k, r := keys(); k != "" || r != "" {
		t.Fatalf("a new row reads (%q, %q), want no keys", k, r)
	}
	// A row that never had a key is not a key node - its caller clears the
	// secret - and nothing is invented for it.
	if reject() {
		t.Error("a row that never held a key reported itself a key node")
	}
	if k, r := keys(); k != "" || r != "" {
		t.Errorf("after rejecting nothing: (%q, %q)", k, r)
	}

	// Trust on first use lands on a row with neither key, and a second first
	// registration, read before the first landed, is refused rather than
	// replacing it.
	first := strings.Repeat("aa", 32)
	if !cas("", "", first) {
		t.Fatal("the first key registration did not land")
	}
	if cas("", "", strings.Repeat("cc", 32)) {
		t.Error("a stale first registration replaced the key")
	}
	if k, _ := keys(); k != first {
		t.Errorf("key = %q, want the first registration", k)
	}

	if !reject() {
		t.Error("a row holding a key was not reported a key node")
	}
	if k, r := keys(); k != "" || r != first {
		t.Errorf("after the reject: (%q, %q), want ('', first)", k, r)
	}
	// A second reset before the node re-pairs keeps the rejected key instead of
	// blanking it, and the row is still a key node.
	if !reject() {
		t.Error("a reset key row stopped being a key node on the second reset")
	}
	if k, r := keys(); k != "" || r != first {
		t.Errorf("after a second reject: (%q, %q), want ('', first)", k, r)
	}

	// A re-pair read before the reset landed is refused; one read after it lands.
	second := strings.Repeat("bb", 32)
	if cas(first, "", second) {
		t.Error("a re-pair read before the reset overwrote the row")
	}
	if !cas("", first, second) {
		t.Fatal("the re-pair of the reset row did not land")
	}
	reject()
	if k, r := keys(); k != "" || r != second {
		t.Errorf("after re-pairing and rejecting again: (%q, %q), want ('', second)", k, r)
	}
}
