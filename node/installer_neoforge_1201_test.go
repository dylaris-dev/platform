package main

import (
	"os"
	"path/filepath"
	"testing"
)

// NeoForge for 1.20.1 is published as net.neoforged:forge:1.20.1-47.x, not
// under net.neoforged:neoforge, so a 47.x build 404ed after a whole pack had
// been downloaded. Checked against maven.neoforged.net.
func TestNeoForgeInstallerURL(t *testing.T) {
	for _, c := range []struct{ version, want string }{
		{"21.1.77", "https://maven.neoforged.net/releases/net/neoforged/neoforge/21.1.77/neoforge-21.1.77-installer.jar"},
		{"47.1.106", "https://maven.neoforged.net/releases/net/neoforged/forge/1.20.1-47.1.106/forge-1.20.1-47.1.106-installer.jar"},
		{"1.20.1-47.1.106", "https://maven.neoforged.net/releases/net/neoforged/forge/1.20.1-47.1.106/forge-1.20.1-47.1.106-installer.jar"},
	} {
		if got := neoForgeInstallerURL(c.version); got != c.want {
			t.Errorf("%s: %s, want %s", c.version, got, c.want)
		}
	}
}

// The 1.20.1 installer writes its args file under net/neoforged/forge, which
// resolveLaunch did not look in, and a later loader install did not clear.
func TestNeoForge1201LaunchDir(t *testing.T) {
	dir := t.TempDir()
	argsDir := filepath.Join(dir, "libraries", "net", "neoforged", "forge", "1.20.1-47.1.106")
	if err := os.MkdirAll(argsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(argsDir, "unix_args.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lf := resolveLaunch(dir)
	if lf.Mode != launchArgfile || lf.ArgsFile != "libraries/net/neoforged/forge/1.20.1-47.1.106/unix_args.txt" {
		t.Fatalf("launch = %+v", lf)
	}
	if err := clearLoaderLaunchDirs(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(argsDir); !os.IsNotExist(err) {
		t.Errorf("the 1.20.1 launch dir survived a loader install (stat err %v)", err)
	}
}
