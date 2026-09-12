package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestACLGoldenVectors MUST match core/services/redisacl/derive_test.go's
// TestGoldenVectors exactly. secret = "0123456789abcdef0123456789abcdef", token = "node-a".
func TestACLGoldenVectors(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	const token = "node-a"
	// The shipper credential is per SERVER, so the vector needs a server uuid too.
	const vectorServer = "srv-1"
	cases := []struct{ name, got, want string }{
		{"node", aclNodePassword(secret, token), "713d2c1b3d181aaee59c162fccc84610e695262c79d2bfb3d738991e0aef8487"},
		{"shipper", aclShipperPassword(secret, token, vectorServer), "16b3871d0705f9100a19d454d4d6c3b8b61d23b75c60206cc5c377f7d231cf55"},
		{"proof", aclProof(secret, token), "dbed444099043c96ff66b685e59a489f532fb41387bcdba74e5b2e382f5f9ec9"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s vector drift vs Core:\n got  %s\n want %s", c.name, c.got, c.want)
		}
	}
	if aclNodeUsername("x") != "node-x" || aclShipperUsername("x", "s1") != "node-x-shipper-s1" {
		t.Fatal("username format must match Core")
	}
}

func TestNodeSecretRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := loadNodeSecret(dir); ok {
		t.Fatal("expected no secret in empty dir")
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	if err := saveNodeSecret(dir, secret); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok := loadNodeSecret(dir)
	if !ok || string(got) != string(secret) {
		t.Fatalf("round-trip mismatch: ok=%v got=%q", ok, got)
	}
	// malformed file -> ok=false
	_ = os.WriteFile(filepath.Join(dir, ".node_secret"), []byte("nothex"), 0600)
	if _, ok := loadNodeSecret(dir); ok {
		t.Fatal("malformed secret must yield ok=false")
	}
}

// TestFirstBootPersistsIntoAMissingDir is the first-boot case, which is the one
// that actually shipped broken. parseConfig resolves nodeSecretDir, main writes
// the identity + secret into it, and only afterwards does the StorageManager
// MkdirAll that path - so on a fresh volume every save failed with ENOENT, was
// merely WARNed, and the NEXT boot found no cached identity. The node then
// re-enrolled as a brand new node, orphaning the previous row and its three
// Redis ACL users. Observed live: two node rows and three ACL identities for one
// physical node.
func TestFirstBootPersistsIntoAMissingDir(t *testing.T) {
	// Two levels down so a single implicit MkdirAll cannot pass by accident.
	dir := filepath.Join(t.TempDir(), "dylaris_data", "servers")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s must not exist yet (err=%v)", dir, err)
	}

	secret := []byte("0123456789abcdef0123456789abcdef")
	const id = "1f12fc89-2c39-49c4-a630-e2b6d406901b"
	if err := saveNodeSecret(dir, secret); err != nil {
		t.Fatalf("saveNodeSecret into a missing dir: %v", err)
	}
	if err := saveNodeID(dir, id); err != nil {
		t.Fatalf("saveNodeID into a missing dir: %v", err)
	}

	// The point is not that the writes returned nil, it is that the NEXT boot
	// finds them. That read is what decides re-enrol vs reconnect.
	got, ok := loadNodeSecret(dir)
	if !ok || string(got) != string(secret) {
		t.Errorf("secret did not survive: ok=%v got=%q", ok, got)
	}
	if gotID, ok := loadNodeID(dir); !ok || gotID != id {
		t.Errorf("node id did not survive: ok=%v got=%q", ok, gotID)
	}

	// 0755, not 0700: the MC server subdirectories live under this path and
	// MkdirAll leaves an existing directory's mode alone, so tightening it here
	// would silently change permissions for the whole server tree. Windows has
	// no Unix mode bits (MkdirAll always reports 0777 there), so this half only
	// means anything on the platform the node actually ships on.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := fi.Mode().Perm(); perm != 0755 {
			t.Errorf("secret dir mode = %o, want 0755", perm)
		}
	}
}

// The node and Core derive this key in two separate Go modules that cannot
// import each other, so each side pins the shape. A drift makes every SFTP
// login fail with "user not found" and nothing else says why.
func TestSFTPAuthKeyMatchesCore(t *testing.T) {
	if got := sftpAuthKey("n1", "alice"); got != "sftp:auth:n1:alice" {
		t.Errorf("sftpAuthKey = %q, want %q (core: redisacl.SFTPAuthKey)", got, "sftp:auth:n1:alice")
	}
	// Node-scoped by construction: a key that did not carry the token would be
	// readable by every node under the old fleet-wide grant.
	if got := sftpAuthKey("other-node", "alice"); got == sftpAuthKey("n1", "alice") {
		t.Error("sftpAuthKey does not depend on the node token")
	}
}
