package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A setup over an installed sub-server is how the panel changes its software,
// and Core puts the old type and version back in the DB when it fails. The
// setup used to wipe the jars, libraries/ and the loader launch dir first with
// no copy, so a failed download or Forge installer left a server the panel
// showed on its old software with nothing on disk to start it.
func TestSetupOverAnInstalledServerPutsItBackOnFailure(t *testing.T) {
	const (
		oldArgs = "libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt"
		newArgs = "libraries/net/minecraftforge/forge/1.20.1-47.3.0/unix_args.txt"
		oldLib  = "libraries/com/example/lib-1.jar"
		newLib  = "libraries/com/example/lib-2.jar"
	)
	for _, typ := range []string{"forge", "modpack", "technic"} {
		for _, c := range []struct {
			name    string
			success bool
			want    map[string]string // relative to the sub-server
		}{
			{"failed install", false, map[string]string{
				"forge-old.jar":   "old jar",
				"forge-new.jar":   "<missing>",
				oldArgs:           "old args",
				newArgs:           "<missing>",
				oldLib:            "old lib",
				newLib:            "<missing>",
				"world/level.dat": "world",
				"mods/a.jar":      "mod",
			}},
			{"successful install", true, map[string]string{
				"forge-old.jar":   "<missing>",
				"forge-new.jar":   "new jar",
				oldArgs:           "<missing>",
				newArgs:           "new args",
				oldLib:            "<missing>",
				newLib:            "new lib",
				"world/level.dat": "world",
				"mods/a.jar":      "mod",
			}},
		} {
			t.Run(typ+"/"+c.name, func(t *testing.T) {
				serverPath := t.TempDir()
				sub := filepath.Join(serverPath, "server")
				writeTestFile(t, filepath.Join(sub, "forge-old.jar"), "old jar")
				writeTestFile(t, filepath.Join(sub, filepath.FromSlash(oldArgs)), "old args")
				writeTestFile(t, filepath.Join(sub, filepath.FromSlash(oldLib)), "old lib")
				writeTestFile(t, filepath.Join(sub, "world", "level.dat"), "world")
				writeTestFile(t, filepath.Join(sub, "mods", "a.jar"), "mod")

				cfg := InstallerConfig{Type: typ, WipePaths: []string{WipeLibraries, WipeVersions, WipeJars}}
				stash, err := stashBeforeSetup(serverPath, "server", cfg)
				if err != nil || stash == nil {
					t.Fatalf("stashBeforeSetup = %v, %v; want a stash", stash, err)
				}
				// What installServer does next: the wipe, then the installer's output.
				if err := WipeBeforeInstall(sub, cfg.WipePaths); err != nil {
					t.Fatal(err)
				}
				if err := clearLoaderLaunchDirs(sub); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(sub, "forge-new.jar"), "new jar")
				writeTestFile(t, filepath.Join(sub, filepath.FromSlash(newArgs)), "new args")
				writeTestFile(t, filepath.Join(sub, filepath.FromSlash(newLib)), "new lib")

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

// Only a software install over a directory that is already there gets a
// stash. An upload, an upload-zip or a backup archive installs the files
// already in the directory, and moving their jars aside would delete the
// server being installed; a first setup has nothing to keep.
func TestStashBeforeSetupOnlyForASoftwareInstallOverAServer(t *testing.T) {
	for _, c := range []struct {
		typ      string
		existing bool
		want     bool
	}{
		{"paper", true, true},
		{"forge", true, true},
		{"vanilla", true, true},
		{"modpack", true, true},
		{"technic", true, true},
		{"library", true, false},
		{"paper", false, false},
		{"upload", true, false},
		{"upload-zip", true, false},
		{"backup", true, false},
		{"import", true, false},
	} {
		t.Run(c.typ, func(t *testing.T) {
			serverPath := t.TempDir()
			if c.existing {
				writeTestFile(t, filepath.Join(serverPath, "server", "server.jar"), "jar")
			}
			stash, err := stashBeforeSetup(serverPath, "server", InstallerConfig{Type: c.typ})
			if err != nil {
				t.Fatal(err)
			}
			if got := stash != nil; got != c.want {
				t.Fatalf("stash taken = %v, want %v", got, c.want)
			}
			if stash != nil {
				stash.restore()
			}
			if c.existing && readTestFile(filepath.Join(serverPath, "server", "server.jar")) != "jar" {
				t.Error("server.jar is gone")
			}
		})
	}
}

// The setup case is inline in the command switch, so its use of the stash is
// pinned in the source: taken before the install, put back on every failure
// after it, dropped once the server is recreated.
func TestSetupStashesAroundTheInstall(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	m := regexp.MustCompile(`(?s)\n\tcase "setup":(.*?)\n\tcase "reconfigure":`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("setup case not found")
	}
	body := m[1]
	stash := strings.Index(body, "stashBeforeSetup(")
	install := strings.Index(body, "InstallServer(")
	if stash < 0 || install < 0 || stash > install {
		t.Fatal("setup does not stash the installed software before InstallServer")
	}
	after := body[install:]
	// The install, the eula refusal and the start command: three failures.
	if strings.Contains(after, "reportSetupFailed(") || strings.Count(after, "putBack(") < 3 {
		t.Error("a failure after the install reports without putting the old software back")
	}
	if !strings.Contains(after, "stash.discard()") {
		t.Error("setup never drops the stash after a successful install")
	}
}

// The stash is the rollback, not a wipe: a successful setup leaves what the
// owner did not tick to clear. Root jars the install did not replace and
// versions/ stay unless "jars" / "versions" were asked for, and the generated
// caches no token names stay; the loader launch
// dirs go either way, as a Forge/NeoForge install clears them and an args file
// left there would outrank the new jar.
func TestSuccessfulSetupKeepsWhatTheWipeDidNotName(t *testing.T) {
	const oldArgs = "libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt"
	for _, c := range []struct {
		name string
		wipe []string
		want map[string]string
	}{
		{"nothing ticked", nil, map[string]string{
			"helper.jar":              "helper",
			"server.jar":              "new jar",
			"versions/1.20.4/old.jar": "old version",
			"versions/1.21/new.jar":   "new version",
			".fabric/remap.bin":       "cache",
			oldArgs:                   "<missing>",
			"libraries/lib.jar":       "lib",
		}},
		{"jars, versions and libraries ticked", []string{WipeJars, WipeVersions, WipeLibraries}, map[string]string{
			"helper.jar":              "<missing>",
			"server.jar":              "new jar",
			"versions/1.20.4/old.jar": "<missing>",
			"versions/1.21/new.jar":   "new version",
			".fabric/remap.bin":       "cache",
			oldArgs:                   "<missing>",
			"libraries/lib.jar":       "<missing>",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			sub := filepath.Join(serverPath, "server")
			writeTestFile(t, filepath.Join(sub, "helper.jar"), "helper")
			writeTestFile(t, filepath.Join(sub, "server.jar"), "old jar")
			writeTestFile(t, filepath.Join(sub, "versions", "1.20.4", "old.jar"), "old version")
			writeTestFile(t, filepath.Join(sub, ".fabric", "remap.bin"), "cache")
			writeTestFile(t, filepath.Join(sub, filepath.FromSlash(oldArgs)), "old args")
			writeTestFile(t, filepath.Join(sub, "libraries", "lib.jar"), "lib")

			cfg := InstallerConfig{Type: "paper", WipePaths: c.wipe}
			stash, err := stashBeforeSetup(serverPath, "server", cfg)
			if err != nil || stash == nil {
				t.Fatalf("stashBeforeSetup = %v, %v; want a stash", stash, err)
			}
			if err := WipeBeforeInstall(sub, cfg.WipePaths); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(sub, "server.jar"), "new jar")
			writeTestFile(t, filepath.Join(sub, "versions", "1.21", "new.jar"), "new version")
			stash.discard()

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
