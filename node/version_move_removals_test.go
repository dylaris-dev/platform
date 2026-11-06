package main

import (
	"path/filepath"
	"testing"
)

// A move took every old jar from the ONE directory it installs into, so a mod
// whose row recorded the other directory (a Paper server re-declared as Fabric)
// stayed behind on the old version with no row left to remove it. Core now
// names each removal's directory; an older Core's plain names still work.
func TestVersionMoveTakesEachRemovalFromItsOwnDir(t *testing.T) {
	for _, c := range []struct {
		name      string
		pl        versionUpdatePayload
		want      map[string]string
		noPlugins bool
	}{
		{"per-directory removals",
			versionUpdatePayload{Remove: []string{"a.jar", "p.jar"}, RemoveFrom: []versionUpdateRemoval{
				{Dir: "mods", FileName: "a.jar"}, {Dir: "plugins", FileName: "p.jar"}}},
			map[string]string{"mods/a.jar": "<missing>", "plugins/p.jar": "<missing>", "mods/keep.jar": "keep"}, false},
		{"older Core: names only, from the target dir",
			versionUpdatePayload{Remove: []string{"a.jar", "p.jar"}},
			map[string]string{"mods/a.jar": "<missing>", "plugins/p.jar": "old plugin", "mods/keep.jar": "keep"}, false},
		{"a directory that is not there",
			versionUpdatePayload{RemoveFrom: []versionUpdateRemoval{{Dir: "plugins", FileName: "x.jar"}, {Dir: "mods", FileName: "a.jar"}}},
			map[string]string{"mods/a.jar": "<missing>"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			sub := filepath.Join(serverPath, "main")
			writeTestFile(t, filepath.Join(sub, "mods", "a.jar"), "old mod")
			writeTestFile(t, filepath.Join(sub, "mods", "keep.jar"), "keep")
			if !c.noPlugins {
				writeTestFile(t, filepath.Join(sub, "plugins", "p.jar"), "old plugin")
			}
			modsDir := filepath.Join(sub, "mods")
			stash, err := stashServerJars(serverPath, "main")
			if err != nil {
				t.Fatal(err)
			}
			if name, err := takeVersionMoveRemovals(stash, serverPath, "main", "mods", modsDir, c.pl); err != nil {
				t.Fatalf("taking %s: %v", name, err)
			}
			for rel, want := range c.want {
				if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
			// And a failed move puts every one of them back.
			if err := stash.restore(); err != nil {
				t.Fatal(err)
			}
			for rel, want := range map[string]string{"mods/a.jar": "old mod", "plugins/p.jar": "old plugin"} {
				if c.noPlugins && rel == "plugins/p.jar" {
					continue
				}
				if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
					t.Errorf("after restore %s = %q, want %q", rel, got, want)
				}
			}
		})
	}
}
