package main

import (
	"os"
	"path/filepath"
	"testing"
)

// What a successful setup keeps (cache/, versions/, .fabric/) went back
// through MkdirAll as the root node agent, after the install's ownership walk:
// root-owned dirs Paper could not write its new cache/mojang jar into, so the
// server crash-looped after every software change.
func TestSetupKeepsTheDirsItPutsBackTheContainers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	t.Setenv("MC_RUN_AS", "1000")
	serverPath := t.TempDir()
	sub := filepath.Join(serverPath, "server")
	writeTestFile(t, filepath.Join(sub, "server.jar"), "old jar")
	writeTestFile(t, filepath.Join(sub, "cache", "mojang_1.21.1.jar"), "cache")
	writeTestFile(t, filepath.Join(sub, "versions", "1.21.1", "paper.jar"), "old version")
	writeTestFile(t, filepath.Join(sub, ".fabric", "remap", "x.bin"), "remap")
	handInstalledTree(sub)
	if err := os.Chmod(filepath.Join(sub, "cache"), 0o770); err != nil {
		t.Fatal(err)
	}

	stash, err := stashBeforeSetup(serverPath, "server", InstallerConfig{Type: "paper"})
	if err != nil || stash == nil {
		t.Fatalf("stashBeforeSetup = %v, %v; want a stash", stash, err)
	}
	// The install, then InstallServer's own handover, then the discard.
	writeTestFile(t, filepath.Join(sub, "server.jar"), "new jar")
	writeTestFile(t, filepath.Join(sub, "versions", "1.21.4", "paper.jar"), "new version")
	handInstalledTree(sub)
	stash.discard()

	for _, rel := range []string{"cache", "cache/mojang_1.21.1.jar", "versions", "versions/1.21.1", "versions/1.21.1/paper.jar", ".fabric", ".fabric/remap"} {
		fi, err := os.Lstat(filepath.Join(sub, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if !ownedBy(fi, 1000) {
			t.Errorf("%s is not the container's after the setup", rel)
		}
	}
	if fi, err := os.Lstat(filepath.Join(sub, "cache")); err == nil && fi.Mode().Perm() != 0o770 {
		t.Errorf("cache/ mode = %o, want 0770 kept", fi.Mode().Perm())
	}
}
