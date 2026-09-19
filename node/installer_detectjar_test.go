package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The file sets are what the real installers left behind, measured 2026-09-19
// with forge 1.12.2-14.23.5.2860 and 1.16.5-36.2.34 (--installServer).
func TestDetectServerJar(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"forge 1.16.5", []string{"forge-1.16.5-36.2.34-installer.jar", "forge-1.16.5-36.2.34.jar", "minecraft_server.1.16.5.jar"}, "forge-1.16.5-36.2.34.jar"},
		{"forge 1.12.2", []string{"forge-1.12.2-14.23.5.2860.jar", "minecraft_server.1.12.2.jar"}, "forge-1.12.2-14.23.5.2860.jar"},
		{"forge 1.17+ shim wins", []string{"forge-1.20.1-47.2.0-shim.jar", "forge-1.20.1-47.2.0-installer.jar"}, "forge-1.20.1-47.2.0-shim.jar"},
		{"legacy universal", []string{"forge-1.7.10-10.13.4.1614-1.7.10-universal.jar"}, "forge-1.7.10-10.13.4.1614-1.7.10-universal.jar"},
		{"installer alone is not a server", []string{"forge-1.16.5-36.2.34-installer.jar"}, "server.jar"},
		{"paper", []string{"paper-1.21.1-100.jar"}, "paper-1.21.1-100.jar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range c.files {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := DetectServerJar(dir); got != c.want {
				t.Errorf("DetectServerJar = %q, want %q", got, c.want)
			}
		})
	}
}
