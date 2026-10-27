package packs

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

func TestContentTargetBoundaries(t *testing.T) {
	for _, target := range []string{"config/example.toml", "defaultconfigs/test.json", "kubejs/server_scripts/test.js", "kubejs/startup_scripts/start.js"} {
		if !validContentTarget(target) {
			t.Errorf("valid target rejected: %q", target)
		}
	}
	for _, target := range []string{"", ".", "../outside", "config/../../outside", "config//file", "config/./file", "/etc/file", `C:\file`, `config\file`, "config/file:stream", "config/file\x00", "config/"} {
		if validContentTarget(target) {
			t.Errorf("unsafe target accepted: %q", target)
		}
	}
}

func TestModrinthContentMustMatchBuild(t *testing.T) {
	build := &models.PackBuild{Minecraft: "1.20.1", Loader: "forge"}
	version := services.ModrinthVersion{ID: "version", ProjectID: "project", Loaders: []string{"forge"}, GameVersions: []string{"1.20.1"}, Files: []services.ModrinthFile{{Filename: "test.jar", URL: "https://cdn.modrinth.com/data/test.jar", Hashes: map[string]string{"sha1": "a", "sha512": "b"}}}}
	tests := []struct {
		name   string
		mutate func(*services.ModrinthVersion)
		kind   string
		wantOK bool
	}{
		{"matching mod", func(*services.ModrinthVersion) {}, models.ContentTypeMod, true},
		{"different loader", func(v *services.ModrinthVersion) { v.Loaders = []string{"fabric"} }, models.ContentTypeMod, false},
		{"different game", func(v *services.ModrinthVersion) { v.GameVersions = []string{"1.20.2"} }, models.ContentTypeMod, false},
		{"resource pack ignores mod loader", func(v *services.ModrinthVersion) { v.Loaders = []string{"minecraft"} }, models.ContentTypeResourcepack, true},
		{"remote path", func(v *services.ModrinthVersion) { v.Files = []services.ModrinthFile{{Filename: "../test.jar"}} }, models.ContentTypeMod, false},
		{"config cannot be mislabeled as a modrinth type", func(*services.ModrinthVersion) {}, models.ContentTypeConfig, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := version
			tt.mutate(&v)
			err := validateModrinthContent(build, &v, tt.kind)
			if (err == nil) != tt.wantOK {
				t.Fatalf("validation = %v, wantOK %v", err, tt.wantOK)
			}
		})
	}
}
