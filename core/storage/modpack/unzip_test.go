package modpack

import (
	"archive/zip"
	"bytes"
	"testing"
)

// buildZip creates an in-memory zip archive from the given entries (name ->
// content). archive/zip.Writer.Create does not validate entry names, so this
// lets the traversal/unsafe-path tests below construct real archives with
// intentionally malicious entry names.
func buildZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create entry %q: %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

func TestIsUnsafeEntryPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"safe nested path", "mods/example.jar", false},
		{"safe path with backslashes", `mods\subfolder\file.jar`, false},
		{"leading slash is absolute", "/etc/passwd", true},
		{"unc-style double slash", "//server/share/file", true},
		{"parent traversal single segment", "../evil.jar", true},
		{"parent traversal after backslash normalize", `..\evil.jar`, true},
		{"parent traversal embedded mid-path", "a/../../b", true},
		{"parent traversal deep inside safe-looking path", "mods/config/../../../etc/passwd", true},
		{"uppercase windows drive letter", `C:\evil.exe`, true},
		{"lowercase windows drive letter", `d:\evil.exe`, true},
		{"drive-letter-like prefix without slash", "D:file.txt", true},
		{"empty path", "", false},
		{"single char path", "a", false},
		{"dotdot as part of a longer segment, not a traversal", "mods/..config/file.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnsafeEntryPath(tc.path); got != tc.want {
				t.Errorf("IsUnsafeEntryPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestReadZipEntry(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 100)
	zipBytes := buildZip(t, map[string][]byte{
		"sub/file.txt": content,
	})

	t.Run("reads a matching entry under the cap", func(t *testing.T) {
		data, ok := ReadZipEntry(zipBytes, "sub/file.txt", 200)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if !bytes.Equal(data, content) {
			t.Errorf("data mismatch: got %d bytes, want %d bytes", len(data), len(content))
		}
	})

	t.Run("rejects an entry over the declared-size cap without decompressing", func(t *testing.T) {
		_, ok := ReadZipEntry(zipBytes, "sub/file.txt", 50)
		if ok {
			t.Error("expected ok=false when the entry's declared size exceeds maxBytes")
		}
	})

	t.Run("accepts an entry exactly at the cap boundary", func(t *testing.T) {
		data, ok := ReadZipEntry(zipBytes, "sub/file.txt", int64(len(content)))
		if !ok || len(data) != len(content) {
			t.Errorf("expected ok=true with %d bytes, got ok=%v len=%d", len(content), ok, len(data))
		}
	})

	t.Run("matches a backslash-style innerPath against a forward-slash entry", func(t *testing.T) {
		data, ok := ReadZipEntry(zipBytes, `sub\file.txt`, 200)
		if !ok || !bytes.Equal(data, content) {
			t.Errorf("expected the backslash path to normalize and match, ok=%v", ok)
		}
	})

	t.Run("missing entry", func(t *testing.T) {
		_, ok := ReadZipEntry(zipBytes, "sub/does-not-exist.txt", 200)
		if ok {
			t.Error("expected ok=false for a missing entry")
		}
	})

	t.Run("unreadable zip bytes", func(t *testing.T) {
		_, ok := ReadZipEntry([]byte("not a zip"), "sub/file.txt", 200)
		if ok {
			t.Error("expected ok=false for unreadable zip bytes")
		}
	})
}
