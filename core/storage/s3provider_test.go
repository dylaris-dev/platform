package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage/backup"
)

// fakeObjectStore is an in-memory objectStore for S3Provider tests. It is
// local to this file and never redeclared elsewhere.
//
// The failure fields are opt-in and the zero value behaves exactly as it did
// before they existed, so the tests that predate them are unaffected. They let
// the resilience tests in s3resilience_test.go drive a backend that fails a
// bounded number of times and then recovers.
type fakeObjectStore struct {
	m map[string][]byte

	// failErr is returned by the next failLeft operations instead of doing the
	// work. A negative failLeft fails forever, which is how the budget test
	// drives a backend that never comes back.
	failErr  error
	failLeft int

	// attempts counts entries per operation name, so a test can assert that a
	// call was made exactly once rather than merely that it returned an error.
	attempts map[string]int
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{m: map[string][]byte{}, attempts: map[string]int{}}
}

// enter records an attempt and reports the injected failure when one is due.
func (f *fakeObjectStore) enter(op string) error {
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	f.attempts[op]++
	if f.failErr == nil || f.failLeft == 0 {
		return nil
	}
	if f.failLeft > 0 {
		f.failLeft--
	}
	return f.failErr
}

func (f *fakeObjectStore) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	if err := f.enter("Put"); err != nil {
		// Consume part of the reader first. A real Put has already read some of
		// the body by the time the transport fails, which is exactly why a retry
		// would upload only the remainder; this makes the fake fail the same way.
		var head [1]byte
		_, _ = r.Read(head[:])
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.m[key] = b
	return nil
}

func (f *fakeObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if err := f.enter("Get"); err != nil {
		return nil, err
	}
	b, ok := f.m[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeObjectStore) Delete(_ context.Context, key string) error {
	if err := f.enter("Delete"); err != nil {
		return err
	}
	delete(f.m, key)
	return nil
}

func (f *fakeObjectStore) List(_ context.Context, prefix string) ([]backup.Object, error) {
	if err := f.enter("List"); err != nil {
		return nil, err
	}
	var out []backup.Object
	for k, v := range f.m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, backup.Object{Key: k, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeObjectStore) DownloadURL(_ context.Context, key string, _ time.Duration) (string, error) {
	if err := f.enter("DownloadURL"); err != nil {
		return "", err
	}
	return "https://signed.example/" + key, nil
}

func (f *fakeObjectStore) UploadURL(_ context.Context, key string, _ time.Duration) (string, error) {
	if err := f.enter("UploadURL"); err != nil {
		return "", err
	}
	return "https://signed-put.example/" + key, nil
}

func TestS3Provider_WriteGetDelete_AppliesPrefix(t *testing.T) {
	fos := newFakeObjectStore()
	p := &S3Provider{os: fos, prefix: "library"}

	if err := p.WriteFile(context.Background(), "dir/a.txt", strings.NewReader("payload")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, ok := fos.m["library/dir/a.txt"]; !ok {
		t.Fatalf("stored keys = %v, want library/dir/a.txt (prefix applied)", keys(fos.m))
	}
	rc, err := p.GetFile(context.Background(), "dir/a.txt")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "payload" {
		t.Errorf("GetFile = %q, want payload", got)
	}
	if err := p.DeletePath(context.Background(), "dir/a.txt"); err != nil {
		t.Fatalf("DeletePath: %v", err)
	}
	if _, ok := fos.m["library/dir/a.txt"]; ok {
		t.Errorf("key still present after DeletePath")
	}
}

func TestS3Provider_DownloadURL_ReturnsSignedPrefixedKey(t *testing.T) {
	p := &S3Provider{os: newFakeObjectStore(), prefix: "library"}
	url, err := p.DownloadURL(context.Background(), "dir/a.txt", time.Minute)
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	if url != "https://signed.example/library/dir/a.txt" {
		t.Errorf("DownloadURL = %q, want signed prefixed key", url)
	}
}

// The PUT URL must carry the same prefix the read side applies. That prefix is
// where a storage connection's own namespace lives, and the presigned URL is the
// ONLY thing that can transport it: the node's s3 config has no prefix field, so
// a URL signed for the bare key would have the node write next to the prefix
// while Core lists, stats and prunes inside it.
func TestS3Provider_UploadURL_ReturnsSignedPrefixedKey(t *testing.T) {
	p := &S3Provider{os: newFakeObjectStore(), prefix: "server-backups"}
	url, err := p.UploadURL(context.Background(), "backups/srv-1/a.tar.gz", time.Minute)
	if err != nil {
		t.Fatalf("UploadURL: %v", err)
	}
	if url != "https://signed-put.example/server-backups/backups/srv-1/a.tar.gz" {
		t.Errorf("UploadURL = %q, want the signed PUT for the prefixed key", url)
	}
}

func TestS3Provider_ListFiles_SynthesizesImmediateChildren(t *testing.T) {
	fos := newFakeObjectStore()
	p := &S3Provider{os: fos, prefix: "library"}
	_ = p.WriteFile(context.Background(), "root.txt", strings.NewReader("r"))
	_ = p.WriteFile(context.Background(), "mods/one.jar", strings.NewReader("j"))
	_ = p.WriteFile(context.Background(), "mods/two.jar", strings.NewReader("j"))

	files, err := p.ListFiles(context.Background(), "/")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	var names []string
	dirs := map[string]bool{}
	for _, f := range files {
		names = append(names, f.Name)
		if f.IsDir {
			dirs[f.Name] = true
		}
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "mods" || names[1] != "root.txt" {
		t.Fatalf("ListFiles names = %v, want [mods root.txt]", names)
	}
	if !dirs["mods"] {
		t.Errorf("expected mods to be a synthesized dir")
	}
}

func TestS3Provider_CreateDirIsNoop(t *testing.T) {
	p := &S3Provider{os: newFakeObjectStore(), prefix: "library"}
	if err := p.CreateDir(context.Background(), "anything"); err != nil {
		t.Errorf("CreateDir = %v, want nil (no-op for object stores)", err)
	}
}

func TestNewProvider_S3RequiresBucket(t *testing.T) {
	if _, err := NewProvider("s3", "", map[string]string{OptS3AccessKey: "k", OptS3SecretKey: "s"}); err == nil {
		t.Fatal("NewProvider s3 without bucket err = nil, want error")
	}
}

// TestS3Provider_DeletePath_DoesNotDeleteSiblingsWithSharedPrefix is a
// regression test for a real S3-confirmed data-loss bug: listing with a bare
// key prefix (no trailing slash) matches sibling keys too, e.g. prefix
// "library/world" also matches "library/world_nether/level.dat" - a normal
// Minecraft layout. DeletePath must delete the exact key and everything
// nested under it as a directory, and nothing else.
func TestS3Provider_DeletePath_DoesNotDeleteSiblingsWithSharedPrefix(t *testing.T) {
	tests := []struct {
		name       string
		deletePath string
		wantGone   []string
		wantKeep   []string
	}{
		{
			name:       "directory delete does not remove a sibling directory sharing a string prefix",
			deletePath: "world",
			wantGone:   []string{"library/world/level.dat"},
			wantKeep:   []string{"library/world_nether/level.dat", "library/readme.txt"},
		},
		{
			name:       "exact file key delete removes only that key",
			deletePath: "readme.txt",
			wantGone:   []string{"library/readme.txt"},
			wantKeep:   []string{"library/world/level.dat", "library/world_nether/level.dat"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fos := newFakeObjectStore()
			p := &S3Provider{os: fos, prefix: "library"}
			if err := p.WriteFile(context.Background(), "world/level.dat", strings.NewReader("w")); err != nil {
				t.Fatalf("seed world/level.dat: %v", err)
			}
			if err := p.WriteFile(context.Background(), "world_nether/level.dat", strings.NewReader("n")); err != nil {
				t.Fatalf("seed world_nether/level.dat: %v", err)
			}
			if err := p.WriteFile(context.Background(), "readme.txt", strings.NewReader("r")); err != nil {
				t.Fatalf("seed readme.txt: %v", err)
			}

			if err := p.DeletePath(context.Background(), tt.deletePath); err != nil {
				t.Fatalf("DeletePath(%q): %v", tt.deletePath, err)
			}

			for _, k := range tt.wantGone {
				if _, ok := fos.m[k]; ok {
					t.Errorf("key %q still present after DeletePath(%q), want deleted", k, tt.deletePath)
				}
			}
			for _, k := range tt.wantKeep {
				if _, ok := fos.m[k]; !ok {
					t.Errorf("key %q missing after DeletePath(%q), want kept (sibling must survive)", k, tt.deletePath)
				}
			}
		})
	}
}

// TestS3Provider_CopyToLocal_NonZipDirectory_DoesNotCopySiblingPrefix covers
// the non-zip directory branch of CopyToLocal: it must copy only the objects
// under the source prefix, never a sibling whose key merely starts with the
// same string (e.g. "mods" must not also pull in "modsBackup/x.jar").
//
// This asserts the EXACT set of files that end up under destPath, not just
// the absence of a couple of guessed leak paths. CopyToLocal's rel-name
// computation, `strings.TrimPrefix(strings.TrimPrefix(o.Key, base), "/")`,
// strips the literal string `base` ("library/mods"), not a slash-bounded
// path segment. So under the pre-pathObjects bug, a leaked
// "library/modsBackup/x.jar" would NOT land at destPath/x.jar or under
// destPath/modsBackup/ - it lands at destPath/Backup/x.jar (only the
// substring "library/mods" is stripped, leaving "Backup/x.jar"). A check
// that only looks for x.jar or a modsBackup dir never notices that and
// passes regardless of the bug. Walking the whole tree and comparing the
// full relative-path set catches any leak wherever it lands.
func TestS3Provider_CopyToLocal_NonZipDirectory_DoesNotCopySiblingPrefix(t *testing.T) {
	fos := newFakeObjectStore()
	p := &S3Provider{os: fos, prefix: "library"}
	seed := map[string]string{
		"mods/a.jar":       "a-content",
		"mods/b.jar":       "b-content",
		"modsBackup/x.jar": "x-content",
	}
	for k, v := range seed {
		if err := p.WriteFile(context.Background(), k, strings.NewReader(v)); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	dst := t.TempDir()
	if err := p.CopyToLocal(context.Background(), "mods", dst); err != nil {
		t.Fatalf("CopyToLocal: %v", err)
	}

	var got []string
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil {
			return err
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk dst: %v", err)
	}
	sort.Strings(got)

	want := []string{"a.jar", "b.jar"}
	if len(got) != len(want) {
		t.Fatalf("files under destPath = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files under destPath = %v, want exactly %v", got, want)
		}
	}

	for _, name := range want {
		gotContent, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("read copied %s: %v", name, err)
		}
		wantContent := seed["mods/"+name]
		if string(gotContent) != wantContent {
			t.Errorf("%s content = %q, want %q", name, gotContent, wantContent)
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
