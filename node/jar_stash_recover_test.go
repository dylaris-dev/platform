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

// R22-01: a restore that died between dropping the complete mark and putting
// libraries/ back left a stash that read as partial, so the next recovery
// renamed onto the libraries/ the failed install wrote, failed, and dropped
// the only copy anyway. A dir the stash still holds goes back whole, and a
// stash that cannot be put back is kept rather than deleted.
func TestRecoverStashOfAnInterruptedRestore(t *testing.T) {
	for _, c := range []struct {
		name   string
		mkFail bool // a file where the sub-server dir should be: every rename fails
	}{
		{"mark gone, install output at a taken dir", false},
		{"put-back fails", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			sub := filepath.Join(serverPath, "main")
			stashDir := filepath.Join(serverPath, ".main.oldjars")
			writeTestFile(t, filepath.Join(sub, "libraries", "old.jar"), "old lib")
			writeTestFile(t, filepath.Join(sub, "forge.jar"), "old jar")
			dead, err := stashServerJars(serverPath, "main", "libraries")
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(sub, "libraries", "new.jar"), "new lib")
			dead.root.Close()
			// The interrupted restore: the mark is gone, nothing went back yet.
			os.Remove(filepath.Join(stashDir, stashCompleteMark))
			if c.mkFail {
				if err := os.RemoveAll(sub); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, sub, "not a dir")
			}

			recoverStrandedStash(serverPath, "main")
			if c.mkFail {
				if got := readTestFile(filepath.Join(stashDir, "main", "libraries", "old.jar")); got != "old lib" {
					t.Errorf("the stash that could not be put back was dropped: old.jar = %q", got)
				}
				s, err := stashServerJars(serverPath, "main")
				if s != nil {
					s.root.Close()
				}
				if err == nil {
					t.Error("stashServerJars over a stash it could not put back = nil error")
				}
				if got := readTestFile(filepath.Join(stashDir, "main", "libraries", "old.jar")); got != "old lib" {
					t.Errorf("stashServerJars dropped the stash it could not put back: old.jar = %q", got)
				}
				return
			}
			for rel, want := range map[string]string{
				"libraries/old.jar": "old lib",
				"libraries/new.jar": "<missing>",
				"forge.jar":         "old jar",
			} {
				if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
		})
	}
}

// R22-02: for a sub-server named "complete" the stash's own tree sits at the
// mark's old name, so a partial stash read as complete and recovery deleted
// the root jar it never took. A legacy mark file from an older node still
// counts.
func TestRecoverStashCompleteMark(t *testing.T) {
	for _, c := range []struct {
		name, sub  string
		legacyMark bool
		want       string // the in-place jar after recovery
	}{
		{"partial stash of a sub-server named complete", "complete", false, "in place"},
		{"legacy complete mark file", "main", true, "<missing>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			stashDir := filepath.Join(serverPath, "."+c.sub+".oldjars")
			writeTestFile(t, filepath.Join(serverPath, c.sub, "server.jar"), "in place")
			writeTestFile(t, filepath.Join(stashDir, c.sub, "versions", "old", "x.jar"), "old version")
			writeTestFile(t, filepath.Join(stashDir, stashMovedList), c.sub+"/versions\n")
			if c.legacyMark {
				writeTestFile(t, filepath.Join(stashDir, "complete"), "")
			}

			recoverStrandedStash(serverPath, c.sub)
			if got := readTestFile(filepath.Join(serverPath, c.sub, "server.jar")); got != c.want {
				t.Errorf("server.jar = %q, want %q", got, c.want)
			}
			if got := readTestFile(filepath.Join(serverPath, c.sub, "versions", "old", "x.jar")); got != "old version" {
				t.Errorf("versions/old/x.jar = %q, want it back", got)
			}
		})
	}
}
