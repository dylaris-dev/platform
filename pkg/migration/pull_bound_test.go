package migration

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// The pull used to stream the whole body to disk and only then check the
// sha256, so a source that keeps sending fills the target's storage path first
// and fails the hash afterwards. That path holds every other tenant's server on
// that node, and the node's own disk-full guards then stop unrelated servers
// from starting.
//
// The source is not always platform hardware: a BYON node is the customer's own
// machine, and it is the side that decides how many bytes to send.
//
// The assertion that matters is how much reached the disk, not that an error
// came back: the size check alone still errors - after the bytes have landed.
func TestPullRefusesMoreThanTheAnnouncedSize(t *testing.T) {
	const announced = 1024
	var sent atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64*1024)
		for i := 0; i < 4096; i++ { // 256MB, far past the announced size
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.zip")
	err := PullURL(context.Background(), srv.URL, strings.Repeat("0", 64), dest, 0, announced)
	if err == nil {
		t.Fatal("an endless response was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds the announced") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
	// Generous: the reader stops at announced+1, but the sender is already a
	// few socket buffers ahead. 8MB is far below the 256MB it would otherwise
	// push, and far above any plausible buffering.
	if n := sent.Load(); n > 8<<20 {
		t.Errorf("the source pushed %d bytes past a %d byte limit; the download is not bounded", n, announced)
	}
	// The temp file is cleaned up by pullOnce, so nothing of it survives either.
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a refused download still produced %s", dest)
	}
}

// A download of exactly the announced size must still succeed - the cap is the
// size the source staged, not a margin below it.
func TestPullAcceptsExactlyTheAnnouncedSize(t *testing.T) {
	body := []byte(strings.Repeat("z", 5000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	stage := t.TempDir()
	srcDir := filepath.Join(stage, "src")
	writeFile(t, filepath.Join(srcDir, "f.txt"), body)
	zipPath := filepath.Join(stage, "s.zip")
	sum, size, err := Archive(srcDir, zipPath)
	if err != nil {
		t.Fatal(err)
	}

	zipped, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(zipped)
	}))
	defer srv2.Close()

	dest := filepath.Join(t.TempDir(), "out.zip")
	if err := PullURL(context.Background(), srv2.URL, sum, dest, 0, size); err != nil {
		t.Fatalf("a correctly sized archive was refused: %v", err)
	}
}

// 0 means no cap: an older core sends no size, and the migration must keep
// working rather than fail closed on a field the other side does not send yet.
func TestPullWithoutAnAnnouncedSizeIsUnbounded(t *testing.T) {
	body := []byte("some archive bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.zip")
	// A deliberately wrong hash, so the only thing under test is that the copy
	// itself is not refused for its size.
	err := PullURL(context.Background(), srv.URL, strings.Repeat("0", 64), dest, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want a hash mismatch (i.e. the body was read in full), got %v", err)
	}
}

// Extract's destination is the tenant's own server directory on a move back to
// a node that still holds the old copy. A symlinked directory there is followed
// by MkdirAll and every entry written under it lands outside.
func TestExtractRefusesToWriteThroughAPlantedDirectorySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}

	// An archive holding world/level.dat.
	srcDir := t.TempDir()
	writeFile(t, filepath.Join(srcDir, "world", "level.dat"), []byte("MIGRATED"))
	zipPath := filepath.Join(t.TempDir(), "s.zip")
	if _, _, err := Archive(srcDir, zipPath); err != nil {
		t.Fatal(err)
	}

	// A destination that already has "world" pointing somewhere else.
	dest := t.TempDir()
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "level.dat")
	writeFile(t, victim, []byte("ANOTHER TENANT"))
	if err := os.Symlink(victimDir, filepath.Join(dest, "world")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	err := Extract(zipPath, dest, math.MaxInt64)
	if err == nil {
		t.Fatal("extract followed a planted directory symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
	if got, rerr := os.ReadFile(victim); rerr != nil || string(got) != "ANOTHER TENANT" {
		t.Fatalf("the archive was written through the link: %q (%v)", got, rerr)
	}
}

// The ordinary case has to keep working: a clean destination, nested
// directories, repeated entries under the same parent.
func TestExtractStillWritesAnOrdinaryTree(t *testing.T) {
	srcDir := t.TempDir()
	writeFile(t, filepath.Join(srcDir, "world", "level.dat"), []byte("A"))
	writeFile(t, filepath.Join(srcDir, "world", "region", "r.0.0.mca"), []byte("B"))
	writeFile(t, filepath.Join(srcDir, "server.properties"), []byte("C"))
	zipPath := filepath.Join(t.TempDir(), "s.zip")
	if _, _, err := Archive(srcDir, zipPath); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := Extract(zipPath, dest, math.MaxInt64); err != nil {
		t.Fatalf("extract: %v", err)
	}
	assertTreesEqual(t, srcDir, dest)
}
