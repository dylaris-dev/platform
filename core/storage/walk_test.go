package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// walkFakeProvider serves a fixed one-level-per-call directory listing so the
// recursion itself is under test, independent of any real backend. Local to
// this file; never redeclared elsewhere.
type walkFakeProvider struct {
	// dirs maps a directory path to its immediate children.
	dirs map[string][]FileInfo
	// listErr, when non-nil for a path, is returned instead of a listing.
	listErr map[string]error
	// calls records every ListFiles argument, in order.
	calls []string
}

func (f *walkFakeProvider) ListFiles(_ context.Context, path string) ([]FileInfo, error) {
	f.calls = append(f.calls, path)
	if err := f.listErr[path]; err != nil {
		return nil, err
	}
	return f.dirs[path], nil
}
func (f *walkFakeProvider) GetFile(context.Context, string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *walkFakeProvider) DeletePath(context.Context, string) error           { return nil }
func (f *walkFakeProvider) CreateDir(context.Context, string) error            { return nil }
func (f *walkFakeProvider) CopyToLocal(context.Context, string, string) error  { return nil }
func (f *walkFakeProvider) WriteFile(context.Context, string, io.Reader) error { return nil }
func (f *walkFakeProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (f *walkFakeProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func TestWalkProvider_RecursesEveryLevel(t *testing.T) {
	// A single ListFiles call returns ONE level only (true of both
	// LocalProvider and S3Provider), so a non-recursive implementation
	// returns just "root.txt" and fails this test.
	f := &walkFakeProvider{dirs: map[string][]FileInfo{
		"": {
			{Name: "root.txt", Size: 3},
			{Name: "mods", IsDir: true},
		},
		"mods": {
			{Name: "one.jar", Size: 10},
			{Name: "nested", IsDir: true},
		},
		"mods/nested": {
			{Name: "deep.cfg", Size: 7},
		},
	}}
	got, err := WalkProvider(context.Background(), f, "")
	if err != nil {
		t.Fatalf("WalkProvider err = %v, want nil", err)
	}
	want := []WalkedFile{
		{Key: "mods/nested/deep.cfg", Size: 7},
		{Key: "mods/one.jar", Size: 10},
		{Key: "root.txt", Size: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("WalkProvider returned %d files (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestWalkProvider_SortedAscendingByKey(t *testing.T) {
	f := &walkFakeProvider{dirs: map[string][]FileInfo{
		"": {
			{Name: "zeta.txt", Size: 1},
			{Name: "alpha.txt", Size: 1},
			{Name: "mid", IsDir: true},
		},
		"mid": {{Name: "beta.txt", Size: 1}},
	}}
	got, err := WalkProvider(context.Background(), f, "")
	if err != nil {
		t.Fatalf("WalkProvider err = %v", err)
	}
	var keys []string
	for _, w := range got {
		keys = append(keys, w.Key)
	}
	wantKeys := []string{"alpha.txt", "mid/beta.txt", "zeta.txt"}
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("keys = %v, want %v (ascending)", keys, wantKeys)
	}
}

func TestWalkProvider_EmptyAndMissingRootAreNotErrors(t *testing.T) {
	f := &walkFakeProvider{dirs: map[string][]FileInfo{"": {}}}
	got, err := WalkProvider(context.Background(), f, "")
	if err != nil {
		t.Fatalf("WalkProvider on empty root err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("WalkProvider on empty root = %+v, want no files", got)
	}
}

func TestWalkProvider_KeysAreRelativeToRoot(t *testing.T) {
	f := &walkFakeProvider{dirs: map[string][]FileInfo{
		"library": {{Name: "sub", IsDir: true}},
		"library/sub": {
			{Name: "a.jar", Size: 4},
		},
	}}
	got, err := WalkProvider(context.Background(), f, "library")
	if err != nil {
		t.Fatalf("WalkProvider err = %v", err)
	}
	if len(got) != 1 || got[0].Key != "sub/a.jar" {
		t.Fatalf("WalkProvider(root=library) = %+v, want [{sub/a.jar 4}] (keys relative to root)", got)
	}
}

func TestWalkProvider_PropagatesListError(t *testing.T) {
	boom := errors.New("backend 503")
	f := &walkFakeProvider{
		dirs:    map[string][]FileInfo{"": {{Name: "mods", IsDir: true}}},
		listErr: map[string]error{"mods": boom},
	}
	if _, err := WalkProvider(context.Background(), f, ""); !errors.Is(err, boom) {
		t.Fatalf("WalkProvider err = %v, want it to wrap %v (a backend error is NOT an empty dir)", err, boom)
	}
}

func TestWalkProvider_DepthCapIsEnforced(t *testing.T) {
	// Build a tree deeper than MaxWalkDepth: every level holds one dir.
	dirs := map[string][]FileInfo{}
	path := ""
	for i := 0; i <= MaxWalkDepth+2; i++ {
		child := fmt.Sprintf("d%d", i)
		dirs[path] = []FileInfo{{Name: child, IsDir: true}}
		if path == "" {
			path = child
		} else {
			path = path + "/" + child
		}
	}
	dirs[path] = []FileInfo{{Name: "leaf.txt", Size: 1}}
	f := &walkFakeProvider{dirs: dirs}
	if _, err := WalkProvider(context.Background(), f, ""); !errors.Is(err, ErrWalkTooDeep) {
		t.Fatalf("WalkProvider err = %v, want ErrWalkTooDeep", err)
	}
}

func TestWalkProvider_AgainstLocalProvider(t *testing.T) {
	// End-to-end against the real LocalProvider, rooted in a temp dir so
	// nothing is written into the package working directory.
	p := &LocalProvider{BasePath: t.TempDir()}
	for _, f := range []struct{ key, body string }{
		{"top.txt", "aaa"},
		{"a/one.txt", "bb"},
		{"a/b/two.txt", "c"},
	} {
		if err := p.WriteFile(context.Background(), f.key, strings.NewReader(f.body)); err != nil {
			t.Fatalf("WriteFile %s: %v", f.key, err)
		}
	}
	got, err := WalkProvider(context.Background(), p, "")
	if err != nil {
		t.Fatalf("WalkProvider err = %v", err)
	}
	want := []WalkedFile{
		{Key: "a/b/two.txt", Size: 1},
		{Key: "a/one.txt", Size: 2},
		{Key: "top.txt", Size: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("WalkProvider = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
