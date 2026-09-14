package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
)

// probeFakeStorage is an in-memory backupstorage.Storage for exercising the
// round-trip probe without a real backend. getBytes, when set, forces Get to
// return content that differs from what was written, to drive the mismatch path.
type probeFakeStorage struct {
	objects  map[string][]byte
	putErr   error
	getErr   error
	getBytes []byte
}

func (f *probeFakeStorage) Provider() string { return "local" }

func (f *probeFakeStorage) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	if f.putErr != nil {
		return f.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[key] = b
	return nil
}

func (f *probeFakeStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getBytes != nil {
		return io.NopCloser(bytes.NewReader(f.getBytes)), nil
	}
	b, ok := f.objects[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *probeFakeStorage) Delete(_ context.Context, key string) error {
	delete(f.objects, key)
	return nil
}

func (f *probeFakeStorage) List(context.Context, string) ([]backupstorage.Object, error) {
	return nil, nil
}

func (f *probeFakeStorage) Stat(context.Context, string) (backupstorage.Object, error) {
	return backupstorage.Object{}, fs.ErrNotExist
}

func (f *probeFakeStorage) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (f *probeFakeStorage) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (*probeFakeStorage) CreateMultipart(context.Context, string) (string, error) {
	return "", backupstorage.ErrMultipartUnsupported
}

func (*probeFakeStorage) UploadPartURL(context.Context, string, string, int32, time.Duration) (string, error) {
	return "", backupstorage.ErrMultipartUnsupported
}

func (*probeFakeStorage) CompleteMultipart(context.Context, string, string, int64) (int64, error) {
	return 0, backupstorage.ErrMultipartUnsupported
}

func (*probeFakeStorage) AbortMultipart(context.Context, string, string) error {
	return backupstorage.ErrMultipartUnsupported
}

func TestProbeBackupStorage_HappyPathDeletesProbe(t *testing.T) {
	f := &probeFakeStorage{}
	ok, msg := probeBackupStorage(context.Background(), f)
	if !ok {
		t.Fatalf("expected ok, got: %s", msg)
	}
	if len(f.objects) != 0 {
		t.Errorf("probe object left behind: %v", f.objects)
	}
}

func TestProbeBackupStorage_ReadBackMismatchFails(t *testing.T) {
	f := &probeFakeStorage{getBytes: []byte("something else entirely")}
	ok, msg := probeBackupStorage(context.Background(), f)
	if ok {
		t.Fatal("expected failure when the backend hands back different bytes")
	}
	if !strings.Contains(msg, "not consistent") {
		t.Errorf("message = %q, want a consistency error", msg)
	}
	if len(f.objects) != 0 {
		t.Errorf("probe object left behind after mismatch: %v", f.objects)
	}
}

func TestProbeBackupStorage_PutFailureFails(t *testing.T) {
	f := &probeFakeStorage{putErr: errors.New("disk full")}
	ok, msg := probeBackupStorage(context.Background(), f)
	if ok {
		t.Fatal("expected failure on put error")
	}
	if !strings.Contains(msg, "put failed") {
		t.Errorf("message = %q, want a put error", msg)
	}
}

func TestProbeBackupStorage_ReadFailureFails(t *testing.T) {
	f := &probeFakeStorage{getErr: errors.New("connection reset")}
	ok, msg := probeBackupStorage(context.Background(), f)
	if ok {
		t.Fatal("expected failure on read-back error")
	}
	if !strings.Contains(msg, "read-back failed") {
		t.Errorf("message = %q, want a read-back error", msg)
	}
}

func TestBackupStorageEphemeralWarning(t *testing.T) {
	orig := pathOnContainerRootFS
	t.Cleanup(func() { pathOnContainerRootFS = orig })

	cases := []struct {
		name          string
		provider      string
		config        string
		onRoot        bool
		determinable  bool
		wantWarn      bool
		wantConsulted bool
	}{
		{"local ephemeral warns", "local", `{"basePath":"/data/backups"}`, true, true, true, true},
		{"shared ephemeral warns", "shared", `{"basePath":"/data/backups"}`, true, true, true, true},
		{"local mounted does not warn", "local", `{"basePath":"/mnt/nas"}`, false, true, false, true},
		{"undeterminable does not warn", "local", `{"basePath":"/x"}`, true, false, false, true},
		// s3 has no local path, so the check must not even be consulted.
		{"s3 never consults the check", "s3", `{"bucket":"b"}`, true, true, false, false},
		{"unparseable config does not warn", "local", `not json`, true, true, false, false},
		{"empty basePath does not warn", "local", `{"basePath":""}`, true, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			consulted := false
			pathOnContainerRootFS = func(string) (bool, bool) {
				consulted = true
				return c.onRoot, c.determinable
			}
			s := &models.BackupStorage{Provider: c.provider, Config: json.RawMessage(c.config)}
			warn := backupStorageEphemeralWarning(s)
			if (warn != "") != c.wantWarn {
				t.Errorf("warning=%q, wantWarn=%v", warn, c.wantWarn)
			}
			if consulted != c.wantConsulted {
				t.Errorf("seam consulted=%v, want=%v", consulted, c.wantConsulted)
			}
		})
	}
}
