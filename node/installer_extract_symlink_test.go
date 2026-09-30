package main

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeZip builds an archive with the given name -> body entries.
func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}

// mustSymlink plants a symlink, skipping the test where the OS will not allow it.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" || errors.Is(err, os.ErrPermission) {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}
}

// The extraction destination is a directory the TENANT owns: they write to it
// over SFTP, over beam, and from inside their own Minecraft container, which has
// the server directory bind-mounted. A symlink planted there is followed by
// os.OpenFile on the NODE's side, so an archive entry underneath it lands
// wherever the link points.
//
// The lexical prefix check the extractors used cannot see this - it cleans a
// string and never asks the filesystem. linkStaysWithin was written for exactly
// this attack and the file APIs all take it.
func TestExtractZipDoesNotFollowAPlantedSymlink(t *testing.T) {
	root := t.TempDir()
	// A neighbouring server directory on the same storage path - what a tenant
	// would actually aim at, since os.Rename/os.Create reach it fine.
	neighbour := filepath.Join(root, "neighbour")
	if err := os.MkdirAll(filepath.Join(neighbour, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(neighbour, "plugins", "loaded.jar")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "mine", "server")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, neighbour, filepath.Join(dest, "escape"))

	zipPath := filepath.Join(root, "upload.zip")
	writeZip(t, zipPath, map[string]string{
		"escape/plugins/loaded.jar": "ATTACKER",
		"ok.txt":                    "fine",
	})

	if err := extractZipToDir(zipPath, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read neighbour file: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("the extraction wrote through the symlink into a neighbouring server directory: %q", got)
	}
	// The ordinary entry must still land, or this would be a fix that breaks
	// every legitimate upload.
	if b, err := os.ReadFile(filepath.Join(dest, "ok.txt")); err != nil || string(b) != "fine" {
		t.Fatalf("ordinary entry did not extract: %v / %q", err, b)
	}
}

// Same guard, reached through the .mrpack override extractor. Its check was a
// `strings.Contains(rel, "..")` substring test, which a symlink needs none of.
func TestExtractOverridesDoesNotFollowAPlantedSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "config.yml")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "server")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(dest, "escape"))

	mrpack := filepath.Join(root, "pack.mrpack")
	writeZip(t, mrpack, map[string]string{
		"overrides/escape/config.yml": "ATTACKER",
		"overrides/config/keep.yml":   "fine",
	})

	err := extractOverrides(mrpack, dest)
	if err == nil {
		t.Fatal("extractOverrides accepted an entry that resolves outside destDir")
	}
	if !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "ORIGINAL" {
		t.Fatalf("the override extractor wrote outside destDir: %q", b)
	}
}

// extractRel is what the three extractors share to name an entry inside the
// extraction Root: traversal and the destination itself are rejected here, the
// ordinary case keeps working, and any residual link is refused by the Root at
// creation (covered by the extractor tests above).
func TestExtractRel(t *testing.T) {
	for _, name := range []string{"", "   ", ".", "..", "/", "/.."} {
		if got, err := extractRel(name); err == nil {
			t.Errorf("entry %q was accepted and named %q", name, got)
		}
	}
	// A leading slash is not traversal: it is cleaned away and the entry lands
	// inside the Root, which is what every zip tool does. Traversal collapses to
	// a plain name.
	for name, want := range map[string]string{
		"server.jar":       "server.jar",
		"config/paper.yml": "config/paper.yml",
		"/etc/passwd":      "etc/passwd",
		"a/../b":           "b",
	} {
		got, err := extractRel(name)
		if err != nil {
			t.Errorf("ordinary entry %q was refused: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("entry %q named %q, want %q", name, got, want)
		}
	}
}
