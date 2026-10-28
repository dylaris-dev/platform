package services

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

func mrpackBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const importManifest = `{"formatVersion":1,"game":"minecraft","name":"Bartis Lands","dependencies":{"minecraft":"1.20.1","forge":"47.4.28"},
"files":[
 {"path":"mods/jei.jar","hashes":{"sha1":"aa"},"env":{"client":"required","server":"required"},"downloads":["https://cdn.modrinth.com/data/u6dRKJwZ/versions/abc123/jei.jar"],"fileSize":10},
 {"path":"mods/skinlayers.jar","hashes":{"sha1":"bb"},"env":{"client":"required","server":"unsupported"},"downloads":["https://github.com/x/y/releases/download/1/skinlayers.jar"],"fileSize":5}
]}`

func TestParseMrpackFullReadsManifestAndOverrides(t *testing.T) {
	p, err := ParseMrpackFull(mrpackBytes(t, map[string]string{
		"modrinth.index.json":                 importManifest,
		"overrides/config/jei.toml":           "a=1",
		"client-overrides/options.txt":        "fov:90",
		"server-overrides/config/server.toml": "b=2",
		"unrelated/readme.txt":                "ignored",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Bartis Lands" || p.Dependencies["minecraft"] != "1.20.1" {
		t.Fatalf("manifest = %+v", p)
	}
	if len(p.Files) != 2 || p.Files[0].ModrinthProjectID != "u6dRKJwZ" || p.Files[0].ModrinthVersionID != "abc123" || p.Files[0].Side != "both" {
		t.Fatalf("modrinth file = %+v", p.Files)
	}
	if p.Files[1].ModrinthVersionID != "" || p.Files[1].Side != "client" {
		t.Fatalf("non-modrinth client file = %+v", p.Files[1])
	}
	sides := map[string]string{}
	for _, o := range p.Overrides {
		sides[o.Path] = o.Side
	}
	if len(sides) != 3 || sides["config/jei.toml"] != "both" || sides["options.txt"] != "client" || sides["config/server.toml"] != "server" {
		t.Fatalf("overrides = %v", sides)
	}
}

func TestParseMrpackFullRefusesUnsafeAndForeignPacks(t *testing.T) {
	if _, err := ParseMrpackFull(mrpackBytes(t, map[string]string{"overrides/../../etc/passwd": "x", "modrinth.index.json": importManifest})); err == nil {
		t.Fatal("a path climbing out of the instance must be refused")
	}
	if _, err := ParseMrpackFull(mrpackBytes(t, map[string]string{"manifest.json": "{}"})); err == nil {
		t.Fatal("a zip without modrinth.index.json is not a Modrinth pack")
	}
}

func TestMrpackLoader(t *testing.T) {
	for _, tc := range []struct {
		deps        map[string]string
		loader, ver string
	}{
		{map[string]string{"minecraft": "1.21.1", "fabric-loader": "0.16.5"}, "fabric", "0.16.5"},
		{map[string]string{"minecraft": "1.21.1", "neoforge": "21.1.265"}, "neoforge", "21.1.265"},
		{map[string]string{"minecraft": "1.20.1", "forge": "47.4.28"}, "forge", "47.4.28"},
		{map[string]string{"minecraft": "1.21"}, "", ""},
	} {
		if l, v := MrpackLoader(tc.deps); l != tc.loader || v != tc.ver {
			t.Errorf("%v -> %s %s, want %s %s", tc.deps, l, v, tc.loader, tc.ver)
		}
	}
}

func TestDownloadAllowedRefusesOtherHosts(t *testing.T) {
	for _, u := range []string{"http://cdn.modrinth.com/x", "https://example.com/mod.jar", "https://169.254.169.254/latest", "ftp://github.com/x"} {
		if _, err := DownloadAllowed(context.Background(), u, 10); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: err = %v, want refusal before any request", u, err)
		}
	}
}
