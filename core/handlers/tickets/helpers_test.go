package tickets

import (
	"dylaris-core/services/corestorage"
	"testing"

	"github.com/pquerna/otp/totp"
)

// Copies of the handlers package's test helpers that the ticket tests used
// while they lived there. Test files cannot be imported across packages.

// The core storage setting keys, by their exported names.
const (
	keyCoreStorageBackend     = corestorage.KeyBackend
	keyCoreStoragePath        = corestorage.KeyPath
	keyCoreStoragePathConfirm = corestorage.KeyPathConfirm
	keyCoreStorageS3Endpoint  = corestorage.KeyS3Endpoint
	keyCoreStorageS3Bucket    = corestorage.KeyS3Bucket
	keyCoreStorageS3AccessKey = corestorage.KeyS3AccessKey
	keyCoreStorageS3SecretKey = corestorage.KeyS3SecretKey
)

// freshTOTPSecret: see handlers/totp_verify_test.go.
func freshTOTPSecret(t *testing.T) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "a"})
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	return key.Secret()
}
