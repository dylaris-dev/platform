package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The node dies while a NeoForge move's installer runs, after the stash was
// complete. Recovery put the stashed files back but left what the dead
// installer wrote beside them: a second launch dir that resolveLaunch takes
// first (21.10 sorts before 21.9) and a new root jar. A complete stash is now
// recovered like restore() does it: the in-place software goes first.
func TestRecoverStashClearsADeadInstallsOutput(t *testing.T) {
	for _, c := range []struct {
		name     string
		complete bool
		want     map[string]string
	}{
		{"complete stash", true, map[string]string{
			"libraries/net/neoforged/neoforge/21.9.1/unix_args.txt":  "old args",
			"libraries/net/neoforged/neoforge/21.10.1/unix_args.txt": "<missing>",
			"neoforge-new.jar":        "<missing>",
			"neoforge-old.jar":        "old jar",
			"versions/new/x.jar":      "<missing>",
			"versions/old/x.jar":      "old version",
			"libraries/other/lib.jar": "lib",
			"world/level.dat":         "world",
		}},
		// A stash that never completed ran no install, so what is in place
		// is the old server and nothing may be cleared.
		{"partial stash", false, map[string]string{
			"libraries/net/neoforged/neoforge/21.9.1/unix_args.txt": "old args",
			"neoforge-old.jar":   "old jar",
			"versions/old/x.jar": "old version",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			sub := filepath.Join(serverPath, "main")
			writeTestFile(t, filepath.Join(sub, "libraries", "net", "neoforged", "neoforge", "21.9.1", "unix_args.txt"), "old args")
			writeTestFile(t, filepath.Join(sub, "neoforge-old.jar"), "old jar")
			writeTestFile(t, filepath.Join(sub, "versions", "old", "x.jar"), "old version")
			writeTestFile(t, filepath.Join(sub, "libraries", "other", "lib.jar"), "lib")
			writeTestFile(t, filepath.Join(sub, "world", "level.dat"), "world")

			if c.complete {
				dead, err := stashServerJars(serverPath, "main")
				if err != nil {
					t.Fatal(err)
				}
				// The installer's output, then the node dies: no restore.
				writeTestFile(t, filepath.Join(sub, "libraries", "net", "neoforged", "neoforge", "21.10.1", "unix_args.txt"), "new args")
				writeTestFile(t, filepath.Join(sub, "neoforge-new.jar"), "new jar")
				writeTestFile(t, filepath.Join(sub, "versions", "new", "x.jar"), "new version")
				dead.root.Close()
			} else {
				// What a stash that died before it took the root jars holds:
				// only versions/, and the old jar still in place.
				left := filepath.Join(serverPath, ".main.oldjars", "main")
				writeTestFile(t, filepath.Join(left, "versions", "old", "x.jar"), "old version")
				if err := os.RemoveAll(filepath.Join(sub, "versions")); err != nil {
					t.Fatal(err)
				}
			}

			recoverStrandedStash(serverPath, "main")
			for rel, want := range c.want {
				if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
		})
	}
}

// The node dies while a pack update with mods and config ticked is writing
// the new pack. Recovery put the stashed files back one by one into the mods/
// the dead install had started, so old and new jars sat side by side (Forge
// refuses duplicate mods) and an emptied config/ never came back. A directory
// the stash took whole now goes back whole, as restore puts it.
func TestRecoverStashPutsAWholeTakenDirBackWhole(t *testing.T) {
	for _, c := range []struct {
		name   string
		legacy bool // a stash written by a node from before moved.list
		want   map[string]string
	}{
		{"stash lists what it took", false, map[string]string{
			"mods/a.jar":      "old mod",
			"mods/b.jar":      "<missing>",
			"forge.jar":       "old jar",
			"world/level.dat": "world",
		}},
		{"stash from an older node", true, map[string]string{
			"mods/a.jar":      "old mod",
			"forge.jar":       "old jar",
			"world/level.dat": "world",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			sub := filepath.Join(serverPath, "server")
			writeTestFile(t, filepath.Join(sub, "forge.jar"), "old jar")
			writeTestFile(t, filepath.Join(sub, "mods", "a.jar"), "old mod")
			if err := os.MkdirAll(filepath.Join(sub, "config"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(sub, "world", "level.dat"), "world")

			cfg := InstallerConfig{Type: "modpack", WipePaths: []string{WipeMods, WipeConfig, WipeJars}}
			dead, err := stashBeforeSetup(serverPath, "server", cfg)
			if err != nil || dead == nil {
				t.Fatalf("stashBeforeSetup = %v, %v; want a stash", dead, err)
			}
			// The new pack got this far, then the node died: no restore.
			writeTestFile(t, filepath.Join(sub, "mods", "b.jar"), "new mod")
			dead.root.Close()
			if c.legacy {
				os.Remove(filepath.Join(serverPath, ".server.oldjars", stashMovedList))
			}

			recoverStrandedStash(serverPath, "server")
			for rel, want := range c.want {
				if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
			if !c.legacy {
				if st, err := os.Stat(filepath.Join(sub, "config")); err != nil || !st.IsDir() {
					t.Errorf("the empty config/ did not come back: %v", err)
				}
			}
		})
	}
}
