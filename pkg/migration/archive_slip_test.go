package migration

import (
	"archive/zip"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeZip builds a zip file at zipPath containing the given name->content
// entries, in-memory first, then flushed to disk.
func writeZip(t *testing.T, zipPath string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := os.WriteFile(zipPath, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write zip file: %v", err)
	}
}

// TestExtract_ZipSlipRejected proves the zip-slip guard in Extract
// (archive.go ~L148-153): a malicious entry that would resolve outside
// destDir must be rejected with an error, and must never be written to disk.
// Unlike node's installer.go (which silently skips traversal entries),
// migration's Extract fails the whole call with
// `migration: entry %q escapes destination` - this test asserts that REAL
// behavior, not node's.
func TestExtract_ZipSlipRejected(t *testing.T) {
	zipDir := t.TempDir()
	zipPath := filepath.Join(zipDir, "traversal.zip")
	writeZip(t, zipPath, map[string]string{
		"../evil.txt": "evil content",
	})

	destParent := t.TempDir()
	destDir := filepath.Join(destParent, "dest")

	err := Extract(zipPath, destDir, math.MaxInt64)
	if err == nil {
		t.Fatal("Extract: expected error for path-traversal entry, got nil")
	}
	if !strings.Contains(err.Error(), "escapes destination") {
		t.Fatalf("Extract error = %q, want it to mention %q", err.Error(), "escapes destination")
	}

	// The traversal target ("../evil.txt" relative to destDir) is a sibling
	// of destDir, i.e. destParent/evil.txt. It must not exist.
	evilPath := filepath.Join(destParent, "evil.txt")
	if _, statErr := os.Stat(evilPath); !os.IsNotExist(statErr) {
		t.Fatal("zip-slip guard failed: evil.txt was created outside destDir")
	}
}

// TestExtract_DeepZipSlipRejected covers a multi-level traversal
// ("../../evil.txt") to make sure the guard is not just a single ".." check.
func TestExtract_DeepZipSlipRejected(t *testing.T) {
	zipDir := t.TempDir()
	zipPath := filepath.Join(zipDir, "traversal-deep.zip")
	writeZip(t, zipPath, map[string]string{
		"../../evil-deep.txt": "evil content",
	})

	root := t.TempDir()
	destDir := filepath.Join(root, "a", "dest")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := Extract(zipPath, destDir, math.MaxInt64)
	if err == nil {
		t.Fatal("Extract: expected error for deep path-traversal entry, got nil")
	}
	if !strings.Contains(err.Error(), "escapes destination") {
		t.Fatalf("Extract error = %q, want it to mention %q", err.Error(), "escapes destination")
	}

	evilPath := filepath.Join(root, "evil-deep.txt")
	if _, statErr := os.Stat(evilPath); !os.IsNotExist(statErr) {
		t.Fatal("zip-slip guard failed: evil-deep.txt was created outside destDir")
	}
}

// TestExtract_HappyPath is the normal, non-malicious case: every entry lands
// under destDir with matching content.
func TestExtract_HappyPath(t *testing.T) {
	zipDir := t.TempDir()
	zipPath := filepath.Join(zipDir, "normal.zip")
	writeZip(t, zipPath, map[string]string{
		"server.properties": "level-name=world\n",
		"world/level.dat":   "binarydata",
		"config/mod/a.toml": "[a]\nb = 1\n",
	})

	destDir := filepath.Join(t.TempDir(), "dest")
	if err := Extract(zipPath, destDir, math.MaxInt64); err != nil {
		t.Fatalf("Extract: unexpected error: %v", err)
	}

	for name, want := range map[string]string{
		"server.properties": "level-name=world\n",
		"world/level.dat":   "binarydata",
		"config/mod/a.toml": "[a]\nb = 1\n",
	} {
		got, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read extracted %q: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%q content = %q, want %q", name, got, want)
		}
	}
}
