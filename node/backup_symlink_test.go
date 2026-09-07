package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// readArchive returns every regular entry of a gzipped tar as name -> content.
func readArchive(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip open: %v", err)
	}
	defer gr.Close()
	out := map[string]string{}
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(body)
	}
}

// The backup tar is the fourth walker over a tenant's server directory, and it
// was the one that never took zipEntryInfo. The consequence is not the leak the
// zip walkers guard against but a hard stop: Walk reports a link via Lstat, so
// the header is a size-0 symlink entry, and the os.Open that follows copies the
// TARGET's bytes into it. tar answers with ErrWriteTooLong and the walk aborts,
// so every backup of that server fails from then on.
//
// A tenant can plant a link over SFTP or from inside their own container, and a
// modpack installer can plant one by accident.
func TestBackupArchiveSurvivesASymlinkInTheServerDirectory(t *testing.T) {
	serverRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("another tenant's data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "server.properties"), []byte("motd=hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(serverRoot, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(serverRoot, "plugins", "escape.txt")); err != nil {
		// Windows needs developer mode or elevation; the guard is Linux-side.
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(serverRoot, "server.properties"), filepath.Join(serverRoot, "plugins", "inside.txt")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	added, err := writeServerArchive(&buf, serverRoot, serverRoot, nil, nil, nil)
	if err != nil {
		t.Fatalf("a symlink in the server directory failed the whole backup: %v", err)
	}
	if !added {
		t.Fatal("the archive is empty")
	}

	entries := readArchive(t, buf.Bytes())
	if _, ok := entries["plugins/escape.txt"]; ok {
		t.Error("a symlink pointing out of the server directory was archived")
	}
	if entries["server.properties"] != "motd=hi" {
		t.Errorf("server.properties = %q, want %q", entries["server.properties"], "motd=hi")
	}
	// Contained links still archive as their target, otherwise the guard is
	// just "drop every symlink" and legitimate layouts break.
	if entries["plugins/inside.txt"] != "motd=hi" {
		t.Errorf("a contained symlink was dropped or wrong: plugins/inside.txt = %q, want %q",
			entries["plugins/inside.txt"], "motd=hi")
	}
}

// A symlinked DIRECTORY hits the same header-only path: Walk does not descend
// into it, so it arrives as a size-0 entry that os.Open would then read as a
// directory. It has to be dropped, not archived half-written.
func TestBackupArchiveDropsASymlinkedDirectory(t *testing.T) {
	serverRoot := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "loot.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "eula.txt"), []byte("eula=true"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(serverRoot, "worlds")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, serverRoot, serverRoot, nil, nil, nil); err != nil {
		t.Fatalf("a symlinked directory failed the whole backup: %v", err)
	}
	entries := readArchive(t, buf.Bytes())
	if _, ok := entries["worlds/loot.txt"]; ok {
		t.Error("the walk followed a symlinked directory out of the server root")
	}
	if entries["eula.txt"] != "eula=true" {
		t.Error("the ordinary file was lost")
	}
}

// The archive store must never end up inside an archive, and the include and
// exclude patterns have to keep working across the extraction.
func TestBackupArchiveKeepsItsFilters(t *testing.T) {
	serverRoot := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(serverRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("server.properties", "motd=hi")
	write("logs/latest.log", "noise")
	write(backupDirName+"/run-1.tar.gz", "an earlier archive")

	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, serverRoot, serverRoot, nil, []string{"logs/**"}, nil); err != nil {
		t.Fatal(err)
	}
	entries := readArchive(t, buf.Bytes())
	if _, ok := entries[backupDirName+"/run-1.tar.gz"]; ok {
		t.Errorf("%s was archived into the backup it is the store for", backupDirName)
	}
	if _, ok := entries["logs/latest.log"]; ok {
		t.Error("an excluded path was archived")
	}
	if entries["server.properties"] != "motd=hi" {
		t.Error("the included file is missing")
	}
}
