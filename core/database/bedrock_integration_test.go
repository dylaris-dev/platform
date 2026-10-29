package database

import (
	"bytes"
	"testing"

	"dylaris-core/pkg/crypto"
)

// Per-server Floodgate keys: round trip, ciphertext at rest, re-enable keeps
// the first key, a deleted server cascades, and a reseal moves the column.
// Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationBedrockKeys(t *testing.T) {
	db, st := integrationDB(t)
	st.SetBedrockEncryptionKey("cluster-secret-a")
	f := newFixture(t, st)

	if _, ok, err := st.GetBedrockKey(f.server.ID); err != nil || ok {
		t.Fatalf("GetBedrockKey before enable = ok %v, %v", ok, err)
	}

	first := bytes.Repeat([]byte{0xAB}, 16)
	if err := st.EnableBedrock(f.server.ID, first); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetBedrockKey(f.server.ID)
	if err != nil || !ok || !bytes.Equal(got, first) {
		t.Fatalf("GetBedrockKey = %x, %v, %v", got, ok, err)
	}

	var enc string
	if err := db.QueryRow(`SELECT floodgate_key_enc FROM bedrock_servers WHERE server_id = $1`, f.server.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if enc == "" || enc == string(first) || bytes.Contains([]byte(enc), first) {
		t.Fatalf("stored key is not ciphertext: %q", enc)
	}

	// Re-enabling must not rotate a key the server already has.
	if err := st.EnableBedrock(f.server.ID, bytes.Repeat([]byte{0xCD}, 16)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetBedrockKey(f.server.ID); !bytes.Equal(got, first) {
		t.Fatalf("re-enable rotated the key to %x", got)
	}

	list, err := st.ListBedrockServers()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range list {
		if b.ServerID == f.server.ID {
			found = b.ServerUUID == f.server.UUID && bytes.Equal(b.Key, first)
		}
	}
	if !found {
		t.Fatalf("ListBedrockServers missing the server or wrong data: %+v", list)
	}

	var savedEnc string
	if err := db.QueryRow(`SELECT floodgate_key_enc FROM bedrock_servers WHERE server_id = $1`, f.server.ID).Scan(&savedEnc); err != nil {
		t.Fatal(err)
	}
	// The keyless list names the same server and keeps working when a key row
	// cannot be decrypted, which fails the keyed list as a whole.
	assertRef := func() {
		t.Helper()
		refs, err := st.ListBedrockServerRefs()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range refs {
			if r.ServerID == f.server.ID && r.ServerUUID == f.server.UUID {
				return
			}
		}
		t.Fatalf("ListBedrockServerRefs missing the server: %+v", refs)
	}
	assertRef()
	if _, err := db.Exec(`UPDATE bedrock_servers SET floodgate_key_enc = 'garbage' WHERE server_id = $1`, f.server.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListBedrockServers(); err == nil {
		t.Fatal("ListBedrockServers accepted an undecryptable key")
	}
	assertRef()
	if _, err := db.Exec(`UPDATE bedrock_servers SET floodgate_key_enc = $2 WHERE server_id = $1`, f.server.ID, savedEnc); err != nil {
		t.Fatal(err)
	}

	// Reseal moves the column to the new secret.
	if _, err := st.ResealAtRest("cluster-secret-a", "cluster-secret-b"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT floodgate_key_enc FROM bedrock_servers WHERE server_id = $1`, f.server.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	pt, err := crypto.Decrypt(crypto.DeriveKey("cluster-secret-b", "bedrock-floodgate-key"), enc)
	if err != nil || !bytes.Equal(pt, first) {
		t.Fatalf("after reseal: %x, %v", pt, err)
	}
	// Put it back so the store keeps working with its installed key.
	if _, err := st.ResealAtRest("cluster-secret-b", "cluster-secret-a"); err != nil {
		t.Fatal(err)
	}

	if err := st.DisableBedrock(f.server.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetBedrockKey(f.server.ID); ok {
		t.Fatal("key survived DisableBedrock")
	}

	if err := st.EnableBedrock(f.server.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(f.server.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM bedrock_servers WHERE server_id = $1`, f.server.ID).Scan(&n)
	if n != 0 {
		t.Fatal("a deleted server's Floodgate key survived")
	}
}

// With no key installed the write fails closed instead of storing plaintext.
func TestIntegrationBedrockFailsClosedWithoutKey(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	if err := st.EnableBedrock(f.server.ID, []byte("0123456789abcdef")); err == nil {
		t.Fatal("EnableBedrock stored a key with no encryption key installed")
	}
}
