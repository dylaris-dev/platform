package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage/backup"
)

// newAdapterOnTempDir builds the adapter over a real LocalProvider rooted in
// t.TempDir(), so nothing is written into the package working directory.
func newAdapterOnTempDir(t *testing.T) (*CoreStorageBackupAdapter, *LocalProvider) {
	t.Helper()
	p := &LocalProvider{BasePath: t.TempDir()}
	return NewCoreStorageBackupAdapter(p), p
}

func TestCoreStorageBackupAdapter_SatisfiesBackupStorage(t *testing.T) {
	var _ backup.Storage = NewCoreStorageBackupAdapter(&LocalProvider{BasePath: t.TempDir()})
}

func TestCoreStorageBackupAdapter_ProviderName(t *testing.T) {
	a, _ := newAdapterOnTempDir(t)
	if a.Provider() != "core-storage" {
		t.Fatalf("Provider() = %q, want \"core-storage\"", a.Provider())
	}
}

func TestCoreStorageBackupAdapter_PutGetDeleteRoundTrip(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdapterOnTempDir(t)

	if err := a.Put(ctx, "srv-1/2026-07-19.tar.gz", strings.NewReader("archive-bytes"), 13); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := a.Get(ctx, "srv-1/2026-07-19.tar.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "archive-bytes" {
		t.Errorf("Get = %q, want archive-bytes", got)
	}
	if err := a.Delete(ctx, "srv-1/2026-07-19.tar.gz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := a.Get(ctx, "srv-1/2026-07-19.tar.gz"); err == nil {
		t.Fatal("Get after Delete returned nil error, want a missing-key error")
	}
}

func TestCoreStorageBackupAdapter_DeleteIsIdempotent(t *testing.T) {
	// backup.Storage documents Delete as nil-on-missing. LocalProvider's
	// DeletePath is os.RemoveAll (already nil on missing) and S3Provider's
	// returns nil for a missing key, so no wrapper is needed - this locks it in.
	a, _ := newAdapterOnTempDir(t)
	if err := a.Delete(context.Background(), "never/existed.tar.gz"); err != nil {
		t.Fatalf("Delete(missing) = %v, want nil (idempotent per the interface)", err)
	}
}

func TestCoreStorageBackupAdapter_ListIsRecursive(t *testing.T) {
	// StorageProvider.ListFiles is ONE level only, so a non-recursive List
	// would return just "top.tar.gz" and fail here.
	ctx := context.Background()
	a, _ := newAdapterOnTempDir(t)
	for _, f := range []struct{ key, body string }{
		{"top.tar.gz", "aaa"},
		{"srv-1/a.tar.gz", "bb"},
		{"srv-1/nested/b.tar.gz", "c"},
	} {
		if err := a.Put(ctx, f.key, strings.NewReader(f.body), int64(len(f.body))); err != nil {
			t.Fatalf("Put %s: %v", f.key, err)
		}
	}

	objs, err := a.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	sizes := map[string]int64{}
	for _, o := range objs {
		keys = append(keys, o.Key)
		sizes[o.Key] = o.Size
	}
	sort.Strings(keys)
	want := []string{"srv-1/a.tar.gz", "srv-1/nested/b.tar.gz", "top.tar.gz"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("List keys = %v, want %v (recursive)", keys, want)
	}
	if sizes["srv-1/nested/b.tar.gz"] != 1 {
		t.Errorf("nested size = %d, want 1", sizes["srv-1/nested/b.tar.gz"])
	}
}

func TestCoreStorageBackupAdapter_ListUnderPrefixReturnsFullKeys(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdapterOnTempDir(t)
	_ = a.Put(ctx, "srv-1/a.tar.gz", strings.NewReader("x"), 1)
	_ = a.Put(ctx, "srv-2/b.tar.gz", strings.NewReader("y"), 1)

	objs, err := a.List(ctx, "srv-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("List(srv-1) = %+v, want exactly one object", objs)
	}
	if objs[0].Key != "srv-1/a.tar.gz" {
		t.Errorf("List(srv-1)[0].Key = %q, want the FULL key srv-1/a.tar.gz", objs[0].Key)
	}
}

func TestCoreStorageBackupAdapter_StatPresentAndMissing(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdapterOnTempDir(t)
	if err := a.Put(ctx, "srv-1/a.tar.gz", strings.NewReader("12345"), 5); err != nil {
		t.Fatalf("Put: %v", err)
	}

	obj, err := a.Stat(ctx, "srv-1/a.tar.gz")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if obj.Key != "srv-1/a.tar.gz" {
		t.Errorf("Stat Key = %q, want srv-1/a.tar.gz", obj.Key)
	}
	if obj.Size != 5 {
		t.Errorf("Stat Size = %d, want 5 (truthful size without transferring bytes)", obj.Size)
	}
	// Documented, accepted limitation: ListFiles carries no mtime on either
	// backend, so LastModified is the ZERO time. Asserted so it cannot change
	// silently - no current caller reads it off a Stat/List result.
	if !obj.LastModified.IsZero() {
		t.Errorf("Stat LastModified = %v, want the zero time (documented limitation)", obj.LastModified)
	}

	if _, err := a.Stat(ctx, "srv-1/gone.tar.gz"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(missing) err = %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
}

func TestCoreStorageBackupAdapter_StatOnTopLevelKey(t *testing.T) {
	// path.Dir("a.tar.gz") is "."; the adapter must list the provider ROOT,
	// not a literal "." directory.
	ctx := context.Background()
	a, _ := newAdapterOnTempDir(t)
	if err := a.Put(ctx, "a.tar.gz", strings.NewReader("abc"), 3); err != nil {
		t.Fatalf("Put: %v", err)
	}
	obj, err := a.Stat(ctx, "a.tar.gz")
	if err != nil {
		t.Fatalf("Stat(top-level) err = %v, want nil", err)
	}
	if obj.Size != 3 {
		t.Errorf("Stat(top-level) Size = %d, want 3", obj.Size)
	}
}

func TestCoreStorageBackupAdapter_DownloadURLPassesThrough(t *testing.T) {
	// LocalProvider returns ("", nil) so the caller streams via Core.
	a, _ := newAdapterOnTempDir(t)
	url, err := a.DownloadURL(context.Background(), "srv-1/a.tar.gz", time.Minute)
	if err != nil {
		t.Fatalf("DownloadURL err = %v, want nil", err)
	}
	if url != "" {
		t.Errorf("DownloadURL = %q, want \"\" for the path backend", url)
	}
}

// The whole point of this adapter is that it is a PASS-THROUGH, and UploadURL
// used to be the one method that was not: it refused unconditionally, on the
// documented assumption that every caller gated on Provider()=="s3" first.
// services.PrepareNodeStorage does not - it asks any indirection target for an
// upload URL, because resolving the indirection is exactly what a node cannot
// do - so every backup to a saved storage connection failed, R2 included.
func TestCoreStorageBackupAdapter_UploadURLPassesThrough(t *testing.T) {
	t.Run("path backend has no address to PUT to", func(t *testing.T) {
		a, _ := newAdapterOnTempDir(t)
		url, err := a.UploadURL(context.Background(), "srv-1/a.tar.gz", time.Minute)
		if err != nil {
			t.Fatalf("UploadURL err = %v, want nil (the caller turns \"\" into the reason)", err)
		}
		if url != "" {
			t.Errorf("UploadURL = %q, want \"\" for the path backend", url)
		}
	})

	t.Run("s3 backend presigns, prefix included", func(t *testing.T) {
		a := NewCoreStorageBackupAdapter(&S3Provider{os: newFakeObjectStore(), prefix: "server-backups"})
		url, err := a.UploadURL(context.Background(), "backups/srv-1/a.tar.gz", time.Minute)
		if err != nil {
			t.Fatalf("UploadURL err = %v, want nil", err)
		}
		if url != "https://signed-put.example/server-backups/backups/srv-1/a.tar.gz" {
			t.Errorf("UploadURL = %q, want the signed PUT for the prefixed key", url)
		}
	})
}
