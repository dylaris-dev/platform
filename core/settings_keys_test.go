package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every settings key Core reads must be one that something can write.
//
// This is the shape of a defect that had already shipped: /api/tools/beam read
// beam.download_url, a key with no writer anywhere in the tree, so its primary
// branch was unreachable and the fallback 503 told the operator to set exactly
// that key. Nothing errored and nothing logged - the feature was simply never
// in force, which is the hardest kind of defect to notice.
//
// The mirror image (a key the panel saves and displays that no consumer reads)
// is the same family and cost its own round, but the write side is spread over
// []struct{ k, v string } literals that no cheap matcher separates from every
// other two-field literal in the package. This test does not claim to cover
// that direction.

// A read is GetSetting("x"), getSetting("x") or SetSettingBy's read-side twin;
// the key must be a literal, which is the only form worth checking.
var settingsReadRe = regexp.MustCompile(`\b[gG]etSetting\("([a-zA-Z0-9_.]+)"`)

// Keys that are read with no in-tree writer on purpose. Each is a per-deploy
// operator override backed by a working default, not something the panel
// offers - so "nobody can set it from the UI" is the intended state, and the
// default is what is actually in force.
var settingsReadWithoutWriter = map[string]string{
	"beam.release_manifest":     "per-deploy manifest override; defaults to defaultBeamManifestURL",
	"branding.name":             "white-label override served to Beam; defaults to \"Dylaris\"",
	"branding.logo_url":         "white-label override served to Beam; empty means no logo",
	"byon.max_servers_per_core": "operator override for the BYON density cap; defaults to 2",
	"runtime.allowed_images":    "operator override: further image prefixes a server may run, comma-separated; empty means only the platform's own",
	"smtp.":                     "prefix; the full key is concatenated at runtime",
	"smtp.default.":             "prefix; the full key is concatenated at runtime",
}

func goSourceFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no non-test Go sources found; the walk is broken, not the code")
	}
	return files
}

func TestEverySettingReadHasAWriter(t *testing.T) {
	files := goSourceFiles(t)

	// key -> the read sites, so a failure names where to look.
	reads := map[string][]string{}
	sources := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		sources[f] = src
		for _, m := range settingsReadRe.FindAllStringSubmatch(src, -1) {
			reads[m[1]] = append(reads[m[1]], f)
		}
	}
	if len(reads) == 0 {
		t.Fatal("found no settings reads at all; the matcher is broken, not the code")
	}

	var orphans []string
	for key, sites := range reads {
		if _, ok := settingsReadWithoutWriter[key]; ok {
			continue
		}
		// A writer is any other quoted mention of the key: SetSetting,
		// SetSettingBy, or a []struct{ k, v string } entry. Requiring the
		// quotes is what keeps prose - comments, error messages that merely
		// name the key - from passing as a writer, which is exactly how the
		// beam.download_url read looked from a distance.
		quoted := `"` + key + `"`
		found := false
		for _, src := range sources {
			for _, line := range strings.Split(src, "\n") {
				if !strings.Contains(line, quoted) {
					continue
				}
				if settingsReadRe.MatchString(line) && strings.Contains(settingsReadRe.FindString(line), quoted) {
					continue // this line is the read itself
				}
				found = true
				break
			}
			if found {
				break
			}
		}
		if !found {
			sort.Strings(sites)
			orphans = append(orphans, key+" (read in "+strings.Join(sites, ", ")+")")
		}
	}

	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("settings read with no writer anywhere - the branch behind each is unreachable:\n  %s\n"+
			"Either give it a writer or add it to settingsReadWithoutWriter with the default it falls back to.",
			strings.Join(orphans, "\n  "))
	}
}

// An allowlist entry for a key nobody reads any more is dead weight that hides
// the next real one.
func TestSettingsAllowlistHasNoStaleEntries(t *testing.T) {
	files := goSourceFiles(t)
	seen := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range settingsReadRe.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	for key := range settingsReadWithoutWriter {
		if !seen[key] {
			t.Errorf("settingsReadWithoutWriter[%q] is no longer read anywhere; drop it", key)
		}
	}
}
