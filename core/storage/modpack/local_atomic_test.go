package modpack

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
