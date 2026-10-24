package tickets

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gorilla/mux"
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

// posixTestBase: see handlers/core_storage_http_test.go. "/" on a Windows dev
// box, the OS temp dir elsewhere (CI runners are not root).
func posixTestBase() string {
	if runtime.GOOS == "windows" {
		return "/"
	}
	return os.TempDir()
}

// testConnectionProbeDir: see handlers/core_storage_http_test.go.
func testConnectionProbeDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(posixTestBase(), testTmpRootName("dylaris-test-tmp"))
	dir := filepath.ToSlash(filepath.Join(root, t.Name()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return dir
}

// disableAWSRetries: see handlers/core_storage_http_test.go.
func disableAWSRetries(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
}

// freshTOTPSecret: see handlers/totp_verify_test.go.
func freshTOTPSecret(t *testing.T) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "a"})
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	return key.Secret()
}

// staffID and staffRequest: see handlers/staff_bounds_test.go.
const staffID = "33333333-3333-3333-3333-333333333333"

func staffRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", staffID)
	ctx = context.WithValue(ctx, "username", "staff")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// testTmpRootName makes the shared test root per test binary. Every helper
// above removes its whole root on cleanup, and since handlers was split the
// packages that use one run in parallel; a shared name let one package's
// cleanup delete another's directories mid-test.
func testTmpRootName(base string) string { return fmt.Sprintf("%s-%d", base, os.Getpid()) }
