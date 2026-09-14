package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
)

// fakeCoreStorage is a minimal backup.Storage used to prove Open routes the
// "core-storage" provider through Deps.CoreStorage. Local to this file.
type fakeCoreStorage struct {
	gotPrefix string
}

func (f *fakeCoreStorage) Provider() string { return "core-storage" }
func (f *fakeCoreStorage) Put(context.Context, string, io.Reader, int64) error {
	return nil
}
func (f *fakeCoreStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeCoreStorage) Delete(context.Context, string) error { return nil }
func (f *fakeCoreStorage) List(context.Context, string) ([]Object, error) {
	return nil, nil
}
func (f *fakeCoreStorage) Stat(context.Context, string) (Object, error) {
	return Object{}, nil
}
func (f *fakeCoreStorage) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (f *fakeCoreStorage) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", ErrUploadURLUnsupported
}

func (*fakeCoreStorage) CreateMultipart(context.Context, string) (string, error) {
	return "", ErrMultipartUnsupported
}

func (*fakeCoreStorage) UploadPartURL(context.Context, string, string, int32, time.Duration) (string, error) {
	return "", ErrMultipartUnsupported
}

func (*fakeCoreStorage) CompleteMultipart(context.Context, string, string, int64) (int64, error) {
	return 0, ErrMultipartUnsupported
}

func (*fakeCoreStorage) AbortMultipart(context.Context, string, string) error {
	return ErrMultipartUnsupported
}

func TestOpen_CoreStorageUsesDepsBuilderAndSubPrefix(t *testing.T) {
	got := &fakeCoreStorage{}
	deps := Deps{
		CoreStorage: func(subPrefix string) (Storage, error) {
			got.gotPrefix = subPrefix
			return got, nil
		},
	}
	bs := &models.BackupStorage{ID: 3, Name: "Core storage", Provider: "core-storage", Config: json.RawMessage(`{}`)}

	st, err := Open(context.Background(), bs, deps)
	if err != nil {
		t.Fatalf("Open(core-storage) err = %v, want nil", err)
	}
	if st != Storage(got) {
		t.Fatalf("Open returned %T, want the Storage the builder produced", st)
	}
	if got.gotPrefix != CoreStorageSubPrefix {
		t.Errorf("builder called with sub-prefix %q, want %q", got.gotPrefix, CoreStorageSubPrefix)
	}
	if CoreStorageSubPrefix != "server-backups" {
		t.Errorf("CoreStorageSubPrefix = %q, want \"server-backups\"", CoreStorageSubPrefix)
	}
}

func TestOpen_CoreStorageWithoutBuilderFails(t *testing.T) {
	// A caller that never touches core-storage passes a zero Deps; asking for
	// the provider anyway must fail loudly, not return a nil Storage.
	bs := &models.BackupStorage{ID: 3, Provider: "core-storage", Config: json.RawMessage(`{}`)}
	st, err := Open(context.Background(), bs, Deps{})
	if err == nil {
		t.Fatal("Open(core-storage) with no CoreStorage builder err = nil, want an error")
	}
	if st != nil {
		t.Errorf("Open returned a non-nil Storage (%T) alongside the error", st)
	}
}

func TestOpen_CoreStorageBuilderErrorIsWrapped(t *testing.T) {
	boom := errors.New("core storage: backend must be path or s3")
	bs := &models.BackupStorage{ID: 3, Provider: "core-storage", Config: json.RawMessage(`{}`)}
	_, err := Open(context.Background(), bs, Deps{
		CoreStorage: func(string) (Storage, error) { return nil, boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Open err = %v, want it to wrap the builder error %v", err, boom)
	}
}

func TestOpen_ExistingProvidersStillResolve(t *testing.T) {
	// Guards against the new case accidentally shadowing the old ones.
	cases := []struct {
		name     string
		provider string
		cfg      string
		wantErr  bool
	}{
		{"local needs basePath", "local", `{}`, true},
		{"shared needs basePath", "shared", `{}`, true},
		{"node-local needs handles", "node-local", `{}`, true},
		{"unknown provider rejected", "ftp", `{}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bs := &models.BackupStorage{Provider: c.provider, Config: json.RawMessage(c.cfg)}
			_, err := Open(context.Background(), bs, Deps{})
			if (err != nil) != c.wantErr {
				t.Fatalf("Open(%q) err = %v, wantErr %v", c.provider, err, c.wantErr)
			}
		})
	}
}
