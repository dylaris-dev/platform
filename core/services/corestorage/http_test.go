package corestorage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage"
	backupstorage "dylaris-core/storage/backup"
)

// --- MergeCandidate (pure logic backing both SaveConfig and
// TestConnection's "blank secret keeps the stored one" rule) ---

func TestMergeCoreStorageCandidate(t *testing.T) {
	existing := Config{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
		S3SecretKey: "stored-secret",
	}

	cases := []struct {
		name      string
		candidate Config
		want      Config
	}{
		{
			name:      "empty candidate backend reuses the stored config entirely",
			candidate: Config{},
			want:      existing,
		},
		{
			// Fix 1: identity fields (endpoint/bucket/accessKey) all match the
			// stored config, so a blank secret is safe to backfill.
			name: "blank secret + unchanged endpoint/bucket/accessKey reuses the stored secret",
			candidate: Config{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
			want: Config{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k",
				S3SecretKey: "stored-secret",
			},
		},
		{
			// Fix 1 (IMPORTANT): a changed bucket must NOT be silently paired
			// with the old secret - that is a credential-rebinding gap, and it
			// also produces a broken config with no error.
			name: "blank secret + changed bucket does not backfill",
			candidate: Config{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "new", S3AccessKey: "k",
			},
			want: Config{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "new", S3AccessKey: "k",
			},
		},
		{
			name: "blank secret + changed access key does not backfill",
			candidate: Config{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k2",
			},
			want: Config{
				Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "old", S3AccessKey: "k2",
			},
		},
		{
			name: "blank secret + changed endpoint does not backfill",
			candidate: Config{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
			want: Config{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "old", S3AccessKey: "k",
			},
		},
		{
			name: "candidate with its own secret is not overridden",
			candidate: Config{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "new", S3AccessKey: "k2",
				S3SecretKey: "new-secret",
			},
			want: Config{
				Backend: "s3", S3Endpoint: "https://attacker.example.com", S3Bucket: "new", S3AccessKey: "k2",
				S3SecretKey: "new-secret",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergeCandidate(c.candidate, existing)
			if got != c.want {
				t.Errorf("MergeCandidate() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestNormalizeCoreStorageCandidate(t *testing.T) {
	got := NormalizeCandidate(Config{
		Backend: " s3 ", Path: " /mnt/x ", S3Endpoint: " https://e ", S3Bucket: " b ",
		S3AccessKey: " k ", S3SecretKey: " s ",
	})
	want := Config{
		Backend: "s3", Path: "/mnt/x", S3Endpoint: "https://e", S3Bucket: "b",
		S3AccessKey: "k", S3SecretKey: "s",
	}
	if got != want {
		t.Errorf("NormalizeCandidate() = %+v, want %+v", got, want)
	}
}

// --- Fix 1 (credential-rebinding gap): a blank secret must only backfill the
// stored secret when the identity-defining fields (s3Endpoint/s3Bucket/
// s3AccessKey) are unchanged from the stored config. Otherwise the request
// must be rejected instead of silently pairing the real secret with a new
// identity. Covered for both SaveConfig (persists) and TestConnection
// (probes) since MergeCandidate backs both. ---

// --- ProbeProvider (write+read+delete probe logic), exercised
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
	ok, msg := ProbeProvider(context.Background(), fp)
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
	ok, msg := ProbeProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on write failure (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on write failure, want 1 (a failed write can leave a truncated object behind)", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_MismatchStillDeletesProbe(t *testing.T) {
	fp := &fakeProbeProvider{readBack: "not-what-was-written"}
	ok, msg := ProbeProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on mismatch (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on mismatch, want 1 (probe must still be cleaned up)", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_ReadErrorStillDeletesProbe(t *testing.T) {
	fp := &fakeProbeProvider{getErr: errors.New("boom")}
	ok, msg := ProbeProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false on read error (%s)", msg)
	}
	if fp.deleteCalled != 1 {
		t.Errorf("DeletePath called %d times on read error, want 1", fp.deleteCalled)
	}
}

func TestProbeStorageProvider_DeleteFailureAfterMatchReportsFailure(t *testing.T) {
	fp := &fakeProbeProvider{readBack: coreStorageProbePayload, deleteErr: errors.New("locked")}
	ok, msg := ProbeProvider(context.Background(), fp)
	if ok {
		t.Fatalf("ok = true, want false when cleanup delete fails (%s)", msg)
	}
}
