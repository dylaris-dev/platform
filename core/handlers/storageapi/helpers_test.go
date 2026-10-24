package storageapi

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/store"
)

// Copies of handlers test fixtures the storage tests use; each keeps its name.

// coreStorageHTTPFakeStore is a read/write settings fake for the CRUD +
// test-connection handler tests below. Distinct from coreStorageFakeStore
// (core_storage_builder_test.go, GetSetting-only, read-side tests) and from
// modeFakeStore (permissions_mode_test.go, always returns one fixed value):
// this one records SetSetting writes into a map so a save-then-get round
// trip can be asserted.
type coreStorageHTTPFakeStore struct {
	store.Store
	kv map[string]string
}

func newCoreStorageHTTPFakeStore() *coreStorageHTTPFakeStore {
	return &coreStorageHTTPFakeStore{kv: map[string]string{}}
}

func (f *coreStorageHTTPFakeStore) GetSetting(key string) (string, error) { return f.kv[key], nil }

// SetSettingBy is the audited write path; the fake records the value and drops
// the actor, so a handler that writes through it fails its assertion rather than
// panicking on a nil embedded Store.
func (f *coreStorageHTTPFakeStore) SetSettingBy(key, value, _ string) error {
	return f.SetSetting(key, value)
}

func (f *coreStorageHTTPFakeStore) SetSetting(key, value string) error {
	f.kv[key] = value
	return nil
}

func seedCoreStorageS3(fs *coreStorageHTTPFakeStore) {
	fs.kv[handlers.KeyCoreStorageBackend] = "s3"
	fs.kv[handlers.KeyCoreStorageS3Endpoint] = "https://s3.example.com"
	fs.kv[handlers.KeyCoreStorageS3Bucket] = "bucket1"
	fs.kv[handlers.KeyCoreStorageS3AccessKey] = "key1"
	fs.kv[handlers.KeyCoreStorageS3SecretKey] = "stored-secret"
}

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

// testConnectionProbeDir returns an absolute-looking POSIX path ("/...")
// that os.MkdirAll can still create on a Windows dev box (Windows treats a
// leading "/" as drive-root-relative), matching validateCoreStorageConfig's
// deliberately Linux-only absolute-path check (see its doc comment). Same
// convention as the "/mnt/shared" literal already used in
// core_storage_builder_test.go; a real t.TempDir() would fail the "/" check
// on Windows and was deliberately not used here for that reason.
// posixTestBase is where the "/"-prefixed test directories live: the drive
// root on a Windows dev box (see testConnectionProbeDir), the OS temp dir
// everywhere else. Not "/" on Linux: CI runs on GitHub-hosted runners, which
// are not root, so nothing can be created at the filesystem root there.
func posixTestBase() string {
	if runtime.GOOS == "windows" {
		return "/"
	}
	return os.TempDir()
}

// disableAWSRetries forces the AWS SDK to attempt exactly once instead of
// its default 3-attempt retry-with-backoff. Port 1 on 127.0.0.1 refuses the
// connection instantly (no DNS lookup, no listener), but the SDK's default
// retryer still treats a connection-refused dial error as retryable and
// sleeps out an exponential backoff between attempts, which is what made
// these tests take several seconds each despite already targeting loopback.
// AWS_MAX_ATTEMPTS is read by awsconfig.LoadDefaultConfig (used in
// storage/backup/s3.go, a production file this fix does not touch), so
// setting it here is a test-only way to get a single, immediate attempt.
func disableAWSRetries(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
}

// resolveFakeStore serves settings + one storage connection. Embeds store.Store
// so only the two methods the resolver touches need bodies.
type resolveFakeStore struct {
	store.Store
	settings map[string]string
	conn     *models.StorageConnection
	connErr  error
	askedID  int
}

func (f *resolveFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func (f *resolveFakeStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	f.askedID = id
	if f.connErr != nil {
		return nil, f.connErr
	}
	return f.conn, nil
}

// coreStorageFakeStore returns a per-key canned setting value from an
// internal map. This is distinct from modeFakeStore (permissions_mode_test.go),
// which always returns the same value regardless of the requested key and so
// cannot drive the multi-key CoreStorageConfig shape.
type coreStorageFakeStore struct {
	store.Store
	values map[string]string
}

// GetSetting mirrors the real store's behavior for an unset setting: an empty
// value and a nil error.
func (f *coreStorageFakeStore) GetSetting(key string) (string, error) {
	return f.values[key], nil
}

// coreStorageTargetFakeStore is a settings-only store for this file. Local
// here; never redeclared elsewhere.
type coreStorageTargetFakeStore struct {
	store.Store
	kv map[string]string
}

func newCoreStorageTargetFakeStore() *coreStorageTargetFakeStore {
	return &coreStorageTargetFakeStore{kv: map[string]string{}}
}

func (f *coreStorageTargetFakeStore) GetSetting(k string) (string, error) { return f.kv[k], nil }

func (f *coreStorageTargetFakeStore) SetSetting(k, v string) error { f.kv[k] = v; return nil }

// storageReachDepsCounters counts every call into the three ServiceDeps hooks
// that SelfCheck and RunRound touch, so the test below can assert an
// INVOCATION count instead of an outcome.
//
// An outcome-based check (before/after status, or "no round key landed in
// Redis") can be fooled two different ways: an unconfigured backend makes
// SelfCheck produce the exact same idempotent ok result every time (so a
// repeat call is invisible in a status diff), and checkSharedStorageReachable
// short-circuits on "not more than one online Core" before it ever reaches
// the round machinery (so a wrongly-wired round call leaves no round key
// either). A call count sidesteps both: ConfigFor is the first thing observe
// does inside SelfCheck regardless of what it returns, and NewProvider is
// what RunRound's Coordinator calls for ITS OWN participation regardless of
// how many peers there are - so both fire on the very first line of the code
// path they claim to guard, before either masking condition gets a chance to
// apply.
type storageReachDepsCounters struct {
	configFor   atomic.Int64
	onlineCores atomic.Int64
	newProvider atomic.Int64
}

// testTmpRootName makes the shared test root per test binary. Every helper
// above removes its whole root on cleanup, and since handlers was split the
// packages that use one run in parallel; a shared name let one package's
// cleanup delete another's directories mid-test.
func testTmpRootName(base string) string { return fmt.Sprintf("%s-%d", base, os.Getpid()) }
