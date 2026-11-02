package tickets

import (
	"testing"

	"github.com/pquerna/otp/totp"

	"dylaris-core/handlers"
)

// Copies of the handlers package's test helpers that the ticket tests used
// while they lived there. Test files cannot be imported across packages.

// The core storage setting keys, by their exported names.
const (
	keyCoreStorageBackend     = handlers.KeyCoreStorageBackend
	keyCoreStoragePath        = handlers.KeyCoreStoragePath
	keyCoreStoragePathConfirm = handlers.KeyCoreStoragePathConfirm
	keyCoreStorageS3Endpoint  = handlers.KeyCoreStorageS3Endpoint
	keyCoreStorageS3Bucket    = handlers.KeyCoreStorageS3Bucket
	keyCoreStorageS3AccessKey = handlers.KeyCoreStorageS3AccessKey
	keyCoreStorageS3SecretKey = handlers.KeyCoreStorageS3SecretKey
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
