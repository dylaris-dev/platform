package handlers

import (
	"strings"
	"testing"

	"dylaris-core/models"
)

// modrinthEntry is a content entry that qualifies as a clean mrpack files[]
// reference: linked, both hashes, a Modrinth CDN download.
func modrinthEntry(targetPath string) models.BuildContentEntry {
	return models.BuildContentEntry{
		Modversion: models.Modversion{
			SHA1:                "a1",
			SHA512:              "a5",
			ModrinthDownloadURL: "https://cdn.modrinth.com/data/AAAA/versions/1/x.jar",
			TargetPath:          targetPath,
			Filesize:            123,
		},
		Side:    models.SideBoth,
		ModSlug: "sodium",
		Linked:  true,
	}
}

// files[].path is a path the LAUNCHER writes to, so a traversal-bearing target
// path in the manifest is the manifest's version of a zip slip.
//
// renderServerPack's streamModrinthContent already refuses this exact field.
// buildMrpackIndex was a reader with no check, and an exported .mrpack is a file
// that leaves the platform.
func TestTheMrpackManifestRefusesATraversalTargetPath(t *testing.T) {
	pack := &models.Pack{InternalName: "p", InternalSlug: "p"}
	build := &models.PackBuild{VersionString: "1.0", Minecraft: "1.21", Loader: "fabric"}

	for _, bad := range []string{
		"../../../../etc/cron.d/evil",
		"mods/../../../../home/user/.bashrc",
		`mods\..\..\evil.jar`,
		"/etc/passwd",
	} {
		if _, err := buildMrpackIndex(pack, build, []models.BuildContentEntry{modrinthEntry(bad)}); err == nil {
			t.Errorf("target path %q was written into modrinth.index.json unchecked", bad)
		}
	}

	idx, err := buildMrpackIndex(pack, build, []models.BuildContentEntry{modrinthEntry("mods/sodium.jar")})
	if err != nil {
		t.Fatalf("an ordinary target path must still render: %v", err)
	}
	if len(idx.Files) != 1 || idx.Files[0].Path != "mods/sodium.jar" {
		t.Fatalf("files[] = %+v, want the one entry at mods/sodium.jar", idx.Files)
	}
}

// The column is written in exactly one place, and one of its three callers
// hands over third-party text: addModrinthVersion passes the filename the
// Modrinth API reported. Sanitizing in the builder covers every caller at once
// and keeps covering the next one.
func TestTargetPathForNeverEscapesItsDirectory(t *testing.T) {
	tests := []struct {
		name, contentType, fileName, want string
	}{
		{"an ordinary jar", models.ContentTypeMod, "sodium-0.5.jar", "mods/sodium-0.5.jar"},
		{"a config file", models.ContentTypeConfig, "sodium.toml", "config/sodium.toml"},
		{"a resource pack", models.ContentTypeResourcepack, "faithful.zip", "resourcepacks/faithful.zip"},
		{"a relative escape keeps its base name", models.ContentTypeMod, "../../../evil.jar", "mods/evil.jar"},
		{"an escape after a directory", models.ContentTypeMod, "a/../../evil.jar", "mods/evil.jar"},
		{"a windows separator", models.ContentTypeMod, `..\..\evil.jar`, "mods/evil.jar"},
		{"nothing but traversal", models.ContentTypeMod, "../..", "mods/unnamed"},
		{"a bare dot-dot", models.ContentTypeConfig, "..", "config/unnamed"},
		{"an absolute path", models.ContentTypeConfig, "/etc/passwd", "config/passwd"},
		{"nothing at all", models.ContentTypeMod, "", "mods/unnamed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetPathFor(tt.contentType, tt.fileName)
			if got != tt.want {
				t.Errorf("targetPathFor(%q, %q) = %q, want %q", tt.contentType, tt.fileName, got, tt.want)
			}
			if strings.Contains(got, "..") {
				t.Errorf("targetPathFor(%q, %q) = %q, which still escapes", tt.contentType, tt.fileName, got)
			}
		})
	}
}

// The publish warning list and the render must answer the same question about
// the same entry. The list is what the publisher ACKNOWLEDGES ("these will be
// embedded and need redistribution rights"); the render is what actually
// embeds. Two copies of the condition drifting would have the user acknowledge
// one set while a different set shipped.
func TestThePublishWarningAgreesWithWhatTheRenderEmbeds(t *testing.T) {
	entries := []models.BuildContentEntry{
		modrinthEntry("mods/sodium.jar"),       // clean files[] reference
		unlinkedEntry("mods/private-mod.jar"),  // manual upload -> overrides/
		noCDNEntry("mods/self-hosted.jar"),     // linked but no Modrinth CDN URL
		missingHashEntry("mods/no-sha512.jar"), // linked, CDN, but one hash missing
	}

	warned := map[string]bool{}
	for _, p := range nonModrinthContent(entries) {
		warned[p] = true
	}
	for _, e := range entries {
		embedded := !isMrpackFilesEntry(e)
		if warned[e.TargetPath] != embedded {
			t.Errorf("%s: warned=%v but the render embeds=%v", e.TargetPath, warned[e.TargetPath], embedded)
		}
	}
	if len(warned) != 3 {
		t.Errorf("warned about %d entries, want the 3 that get embedded: %v", len(warned), warned)
	}
}

func unlinkedEntry(targetPath string) models.BuildContentEntry {
	e := modrinthEntry(targetPath)
	e.Linked = false
	return e
}

func noCDNEntry(targetPath string) models.BuildContentEntry {
	e := modrinthEntry(targetPath)
	e.ModrinthDownloadURL = "https://example.invalid/x.jar"
	return e
}

func missingHashEntry(targetPath string) models.BuildContentEntry {
	e := modrinthEntry(targetPath)
	e.SHA512 = ""
	return e
}
