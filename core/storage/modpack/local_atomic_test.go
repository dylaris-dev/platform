package modpack

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// failingReader sends one chunk, then asks whether the object is visible yet
// (the copy is mid-way at that point), then fails.
type failingReader struct {
	sent    bool
	visible func() bool
	seen    bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.sent {
		f.seen = f.visible()
		return 0, errors.New("connection dropped")
	}
	f.sent = true
	return copy(p, "partial"), nil
}

// A stream written straight to its final name existed at its partial size for
// the whole copy - Stat said it was there - and a failure mid-copy could leave
// it standing as the object.
func TestLocalPutStreamNeverShowsAPartialObject(t *testing.T) {
	dir := t.TempDir()
	p := &LocalProvider{Paths: []string{dir}}
	ctx := context.Background()

	fr := &failingReader{visible: func() bool {
		_, exists, _ := p.Stat(ctx, "a/obj.zip")
		return exists
	}}
	if err := p.PutStream(ctx, "a/obj.zip", fr, -1); err == nil {
		t.Fatal("a failed stream was reported as stored")
	}
	if fr.seen {
		t.Error("the object was visible while it was still being written")
	}
	if _, exists, _ := p.Stat(ctx, "a/obj.zip"); exists {
		t.Fatal("a failed stream left an object behind")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "a")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	if err := p.PutStream(ctx, "a/obj.zip", strings.NewReader("whole"), 5); err != nil {
		t.Fatal(err)
	}
	rc, _, err := p.Stream(ctx, "a/obj.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "whole" {
		t.Fatalf("stored %q", b)
	}
}

// Put truncated the object in place, so a node streaming the previous render
// got a short or mixed body; and a later mirror's failure removed the earlier
// mirrors' copy, which for an overwrite was the only one left.
func TestLocalPutReplacesWithoutTearingOrLosing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		failMirror bool
	}{{"streaming reader keeps the old object", false}, {"failed mirror keeps the first copy", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := &LocalProvider{Paths: []string{dir}}
			old := strings.Repeat("old render ", 1000)
			if err := p.Put(ctx, "a/pack.mrpack", []byte(old)); err != nil {
				t.Fatal(err)
			}
			if tc.failMirror {
				// A plain file where the second mirror's directory must go.
				blocked := filepath.Join(t.TempDir(), "blocked")
				if err := os.WriteFile(blocked, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				p.Paths = append(p.Paths, blocked)
				if err := p.Put(ctx, "a/pack.mrpack", []byte("new")); err == nil {
					t.Fatal("a failed mirror was reported as stored")
				}
				if _, exists, _ := (&LocalProvider{Paths: []string{dir}}).Stat(ctx, "a/pack.mrpack"); !exists {
					t.Fatal("a failed mirror removed the object the first mirror held")
				}
				return
			}
			if runtime.GOOS == "windows" {
				t.Skip("Windows refuses a rename over an open file; Core ships on Linux")
			}
			rc, size, err := p.Stream(ctx, "a/pack.mrpack")
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			if err := p.Put(ctx, "a/pack.mrpack", []byte("new")); err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			if string(b) != old || int64(len(b)) != size {
				t.Fatalf("an open download read %d bytes of a %d-byte object", len(b), size)
			}
		})
	}
}
