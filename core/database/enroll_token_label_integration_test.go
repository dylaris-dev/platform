package database

import (
	"testing"
	"time"
)

// A node's display name comes from the label its enroll token was minted
// under; the lookup has to find it by the plaintext the node presents, and
// answer "" for a token it does not know. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationEnrollTokenLabelIsFoundByThePlaintext(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	future := time.Now().Add(time.Hour)
	if err := st.CreateNodeEnrollToken(f.user.ID, "label-tok-1", "Home PC", &future, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := st.NodeEnrollTokenLabel("label-tok-1"); err != nil || got != "Home PC" {
		t.Fatalf("label = %q, %v; want Home PC", got, err)
	}
	if got, err := st.NodeEnrollTokenLabel("not-a-token"); err != nil || got != "" {
		t.Fatalf("unknown token: %q, %v; want empty and no error", got, err)
	}
}
