package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// R25-01: a pack writes its overrides over what the owner left unticked
// (config/, mods/), and a failed setup that put back only the stash said the
// old pack was back while its configs were the new pack's and its mods/ held
// both packs' jars. Every file a pack writes now goes through the stash.
func TestPackSetupPutsOverwrittenFilesBackOnFailure(t *testing.T) {
	for _, c := range []struct {
		name    string
		success bool
		want    map[string]string // relative to the sub-server
	}{
		{"failed install", false, map[string]string{
			"config/app.cfg":    "tuned",
			"mods/old.jar":      "old mod",
			"mods/new.jar":      "<missing>",
			"kubejs/startup.js": "<missing>",
			"world/level.dat":   "world",
		}},
		{"successful install", true, map[string]string{
			"config/app.cfg":    "server pack",
			"mods/old.jar":      "old mod",
			"mods/new.jar":      "new mod",
			"kubejs/startup.js": "script",
			"world/level.dat":   "world",
		}},
	} {
		for _, writer := range []string{"modpack overrides", "technic merge"} {
			t.Run(writer+"/"+c.name, func(t *testing.T) {
				serverPath := t.TempDir()
				sub := filepath.Join(serverPath, "server")
				writeTestFile(t, filepath.Join(sub, "server.jar"), "old jar")
				writeTestFile(t, filepath.Join(sub, "config", "app.cfg"), "tuned")
				writeTestFile(t, filepath.Join(sub, "mods", "old.jar"), "old mod")
				writeTestFile(t, filepath.Join(sub, "world", "level.dat"), "world")

				// Config and mods unticked: nothing of theirs is wiped.
				cfg := InstallerConfig{Type: "modpack"}
				stash, err := stashBeforeSetup(serverPath, "server", cfg)
				if err != nil || stash == nil {
					t.Fatalf("stashBeforeSetup = %v, %v; want a stash", stash, err)
				}
				cfg.claim = stash.claim

				if writer == "modpack overrides" {
					mrpack := filepath.Join(t.TempDir(), "pack.mrpack")
					// The same name in both prefixes: the second write must not
					// move the first one's file over the stashed original.
					writeZip(t, mrpack, map[string]string{
						"overrides/config/app.cfg":        "client pack",
						"server-overrides/config/app.cfg": "server pack",
						"overrides/mods/new.jar":          "new mod",
						"overrides/kubejs/startup.js":     "script",
					})
					if err := extractOverrides(mrpack, sub, cfg.claimFile); err != nil {
						t.Fatal(err)
					}
				} else {
					stage := filepath.Join(sub, ".technic-stage-x")
					writeTestFile(t, filepath.Join(stage, "config", "app.cfg"), "server pack")
					writeTestFile(t, filepath.Join(stage, "mods", "new.jar"), "new mod")
					writeTestFile(t, filepath.Join(stage, "kubejs", "startup.js"), "script")
					root, err := os.OpenRoot(sub)
					if err != nil {
						t.Fatal(err)
					}
					err = mergeStage(root, ".technic-stage-x", ".", cfg.claimFile)
					root.Close()
					if err != nil {
						t.Fatal(err)
					}
				}

				if c.success {
					stash.discard()
				} else if err := stash.restore(); err != nil {
					t.Fatalf("restore: %v", err)
				}
				for rel, want := range c.want {
					if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
						t.Errorf("%s = %q, want %q", rel, got, want)
					}
				}
				if _, err := os.Lstat(filepath.Join(serverPath, ".server.oldjars")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the stash is still there: %v", err)
				}
			})
		}
	}
}
