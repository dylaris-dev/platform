package store

import (
	"strings"
	"testing"

	"dylaris-core/pkg/crypto"
)

func sealTOTP(t *testing.T, clusterSecret, secret string) string {
	t.Helper()
	ct, err := crypto.Encrypt(crypto.DeriveKey(clusterSecret, totpSecretPurpose), []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return totpEncMarker + ct
}

// A sealed secret opens under its own key; a plaintext one from before
// encryption reads through; anything that cannot be opened reads as "", which
// validates no code.
func TestDecodeTOTPSecret(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	sealed := sealTOTP(t, "cluster-a", secret)

	keyed := &PostgresStore{}
	keyed.SetTOTPEncryptionKey("cluster-a")
	other := &PostgresStore{}
	other.SetTOTPEncryptionKey("cluster-b")
	none := &PostgresStore{}

	for _, c := range []struct {
		name  string
		s     *PostgresStore
		value string
		want  string
	}{
		{"sealed, right key", keyed, sealed, secret},
		{"legacy plaintext", keyed, secret, secret},
		{"empty", keyed, "", ""},
		{"sealed, other key", other, sealed, ""},
		{"sealed, no key", none, sealed, ""},
		{"marker, garbage", keyed, totpEncMarker + "zz", ""},
	} {
		if got := c.s.decodeTOTPSecret("u1", c.value); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if strings.Contains(sealed, secret) {
		t.Fatal("the sealed value carries the secret in the clear")
	}
}
