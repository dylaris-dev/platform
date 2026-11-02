package handlertest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

// Redis returns a client on a fresh miniredis that is closed when the test
// ends, and the miniredis itself for tests that inspect or fast-forward it.
func Redis(t testing.TB) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

// RedisClient is Redis for tests that only need the client.
func RedisClient(t testing.TB) *redis.Client {
	t.Helper()
	c, _ := Redis(t)
	return c
}

// TempRoot is the per-test-binary root for "/"-prefixed test directories:
// base plus the process id, under the drive root on a Windows dev box
// (Windows treats a leading "/" as drive-root-relative, which is what the
// Linux-only absolute-path checks in core storage accept) and under the OS
// temp dir elsewhere (CI runners are not root, so not "/"). The pid matters:
// the handler packages' tests run in parallel and each cleanup removes its
// whole root, so a shared name let one package delete another's directories
// mid-test.
func TempRoot(base string) string {
	dir := os.TempDir()
	if runtime.GOOS == "windows" {
		dir = "/"
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d", base, os.Getpid()))
}

// ConnectionProbeDir creates and returns an absolute-looking POSIX directory
// for this test under TempRoot("dylaris-test-tmp"), removing the whole root on
// cleanup. A t.TempDir() would fail the "/" check on Windows.
func ConnectionProbeDir(t testing.TB) string {
	t.Helper()
	root := TempRoot("dylaris-test-tmp")
	dir := filepath.ToSlash(filepath.Join(root, t.Name()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return dir
}

// DisableAWSRetries makes the AWS SDK attempt exactly once. Its default
// retryer treats a refused loopback dial as retryable and sleeps out a
// backoff, which made the probe tests take seconds each.
// awsconfig.LoadDefaultConfig reads AWS_MAX_ATTEMPTS.
func DisableAWSRetries(t testing.TB) {
	t.Helper()
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
}

// ReauthPassword is the acting administrator's password in the re-auth
// fixtures; ReauthHash is its bcrypt hash at MinCost, because these run on
// every build. A fixture that exercises the refusal leaves the block out.
const ReauthPassword = "correct-horse-battery-staple"

var ReauthHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte(ReauthPassword), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}()
