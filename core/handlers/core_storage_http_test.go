package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"
)

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

// --- mergeCoreStorageCandidate (pure logic backing both SaveConfig and
// TestConnection's "blank secret keeps the stored one" rule) ---

func TestMergeCoreStorageCandidate(t *testing.T) {
	existing := CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
		S3SecretKey: "stored-secret",
	}

	cases := []struct {
		name      string
		candidate CoreStorageConfig
		want      CoreStorageConfig
	}{
		{
			name:      "empty candidate backend reuses the stored config entirely",
			candidate: CoreStorageConfig{},
			want:      existing,
		},
		{
			// Fix 1: identity fields (endpoint/bucket/accessKey) all match the
			// stored config, so a blank secret is safe to backfill.
			name: "blank secret + unchanged endpoint/bucket/accessKey reuses the stored secret",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
				S3SecretKey: "stored-secret",
			},
		},
		{
			// Fix 1 (IMPORTANT): a changed bucket must NOT be silently paired
			// with the old secret - that is a credential-rebinding gap, and it
			// also produces a broken config with no error.
			name: "blank secret + changed bucket does not backfill",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "new", S3AccessKey: "k",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "new", S3AccessKey: "k",
			},
		},
		{
			name: "blank secret + changed access key does not backfill",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k2",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k2",
			},
		},
		{
			name: "blank secret + changed endpoint does not backfill",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
		},
		{
			name: "candidate with its own secret is not overridden",
			candidate: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "new", S3AccessKey: "k2",
				S3SecretKey: "new-secret",
			},
			want: CoreStorageConfig{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "new", S3AccessKey: "k2",
				S3SecretKey: "new-secret",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeCoreStorageCandidate(c.candidate, existing)
			if got != c.want {
				t.Errorf("mergeCoreStorageCandidate() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestNormalizeCoreStorageCandidate(t *testing.T) {
	got := normalizeCoreStorageCandidate(CoreStorageConfig{
		Backend: " s3 ", Path: " /mnt/x ", S3Endpoint: " https://e ", S3Bucket: " b ",
		S3AccessKey: " k ", S3SecretKey: " s ",
	})
	want := CoreStorageConfig{
		Backend: "s3", Path: "/mnt/x", S3Endpoint: "https://e", S3Bucket: "b",
		S3AccessKey: "k", S3SecretKey: "s",
	}
	if got != want {
		t.Errorf("normalizeCoreStorageCandidate() = %+v, want %+v", got, want)
	}
}

// --- Fix 1 (credential-rebinding gap): a blank secret must only backfill the
// stored secret when the identity-defining fields (s3Endpoint/s3Bucket/
// s3AccessKey) are unchanged from the stored config. Otherwise the request
// must be rejected instead of silently pairing the real secret with a new
// identity. Covered for both SaveConfig (persists) and TestConnection
// (probes) since mergeCoreStorageCandidate backs both. ---

// --- probeStorageProvider (write+read+delete probe logic), exercised
// against a hand-written fake so the mismatch/error cleanup paths are
// deterministic and do not depend on a real filesystem or network backend. ---

type fakeProbeProvider struct {
	readBack     string
	getErr       error
	writeErr     error
	deleteErr    error
	deleteCalled int
}

func (f *fakeProbeProvider) ListFiles(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (f *fakeProbeProvider) CreateDir(context.Context, string) error           { return nil }
func (f *fakeProbeProvider) CopyToLocal(context.Context, string, string) error { return nil }
func (f *fakeProbeProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (f *fakeProbeProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (*fakeProbeProvider) CreateMultipart(context.Context, string) (string, error) {
	return "", backupstorage.ErrMultipartUnsupported
}

func (*fakeProbeProvider) UploadPartURL(context.Context, string, string, int32, time.Duration) (string, error) {
	return "", backupstorage.ErrMultipartUnsupported
}

func (*fakeProbeProvider) CompleteMultipart(context.Context, string, string, int64) (int64, error) {
	return 0, backupstorage.ErrMultipartUnsupported
}

func (*fakeProbeProvider) AbortMultipart(context.Context, string, string) error {
	return backupstorage.ErrMultipartUnsupported
}

func (*fakeProbeProvider) ListMultipart(context.Context, string, string) (backupstorage.MultipartUsage, error) {
	return backupstorage.MultipartUsage{}, backupstorage.ErrMultipartUnsupported
}
func (f *fakeProbeProvider) WriteFile(context.Context, string, io.Reader) error {
	return f.writeErr
}

func (f *fakeProbeProvider) GetFile(context.Context, string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return io.NopCloser(strings.NewReader(f.readBack)), nil
}

func (f *fakeProbeProvider) DeletePath(context.Context, string) error {
	f.deleteCalled++
	return f.deleteErr
}

func TestProbeStorageProvider_Success(t *testing.T) {
	fp := &fakeProbeProvider{readBack: coreStorageProbePayload}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if !ok {
		t.Fatalf("ok = false, want true (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times, want 1", fp.deleteCalled)
	}
}

// TestProbeStorageProvider_WriteFailureCleansUpProbe guards Fix 3: the
// write-failure path must attempt cleanup like every other return path,
// because a backend is not required to leave nothing behind after a failed
// write. LocalProvider happens to be tidy about it now (it stages and
// renames); the probe does not get to assume that of the provider it was
// handed.
func TestProbeStorageProvider_WriteFailureCleansUpProbe(t *testing.T) {
	fp := &fakeProbeProvider{writeErr: errors.New("disk full")}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on write failure (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on write failure, want 1 (a failed write can leave a truncated object behind)", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_MismatchStillDeletesProbe(t *testing.T) {
	fp := &fakeProbeProvider{readBack: "not-what-was-written"}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on mismatch (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on mismatch, want 1 (probe must still be cleaned up)", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_ReadErrorStillDeletesProbe(t *testing.T) {
	fp := &fakeProbeProvider{getErr: errors.New("boom")}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on read error (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on read error, want 1", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_DeleteFailureAfterMatchReportsFailure(t *testing.T) {
	fp := &fakeProbeProvider{readBack: coreStorageProbePayload, deleteErr: errors.New("locked")}
	ok, msg := probeStorageProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false when cleanup delete fails (%s)", msg)
	}
}

// testTmpRootName makes the shared test root per test binary. Every helper
// above removes its whole root on cleanup, and since handlers was split the
// packages that use one run in parallel; a shared name let one package's
// cleanup delete another's directories mid-test.
func testTmpRootName(base string) string { return fmt.Sprintf("%s-%d", base, os.Getpid()) }
