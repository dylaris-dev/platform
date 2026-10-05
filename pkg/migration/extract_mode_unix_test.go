//go:build unix

package migration

import (
	"archive/zip"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The archive's modes are the sender's. Extraction runs as root, so a setuid
// bit kept from it gave a root-owned setuid file on the target.
func TestExtractDropsSpecialModeBits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "in.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	h := &zip.FileHeader{Name: "bin/x", Method: zip.Store}
	h.SetMode(0o755 | fs.ModeSetuid | fs.ModeSetgid)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	zw.Close()
	f.Close()

	dest := t.TempDir()
	if err := Extract(p, dest, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dest, "bin", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		t.Fatalf("mode %v kept the archive's special bits", fi.Mode())
	}
}
