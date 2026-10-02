package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// An archive that unpacks to more than the disk holds filled it: nothing but a
// project quota stopped it, and not every node has one.
func TestUnpackingStopsAtTheDiskBudget(t *testing.T) {
	prev := restoreDiskBudget
	restoreDiskBudget = func(string) int64 { return 8 }
	t.Cleanup(func() { restoreDiskBudget = prev })
	dest := t.TempDir()
	zipWith(t, filepath.Join(dest, ".upload.zip"), map[string]string{"big.dat": "sixteen bytes!!!"})
	if err := installFromUploadZip(dest, "flat"); err == nil {
		t.Fatal("an archive larger than the budget was unpacked")
	}
	if st, err := os.Stat(filepath.Join(dest, "big.dat")); err == nil && st.Size() > 9 {
		t.Fatalf("big.dat is %d bytes", st.Size())
	}
}

func zipWith(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
