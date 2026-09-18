package handlers

import (
	"os"
	"strings"
	"testing"
)

// safeKeyComponent exists because "Modrinth version numbers reach us verbatim
// (no charset validation), so a crafted '../..' could otherwise address another
// tenant's objects". Only versionString was ever checked against it. The other
// three build fields travel with the build into the .mrpack's dependencies, and
// a node installing the pack fmt.Sprintf's minecraft and the loader version
// into a meta.fabricmc.net path with no escaping (node/installer.go).
func TestBuildKeyComponentsAreCheckedLikeVersionString(t *testing.T) {
	tests := []struct {
		name                             string
		minecraft, loader, loaderVersion string
		wantBad                          string
	}{
		{name: "an ordinary fabric build", minecraft: "1.21.1", loader: "fabric", loaderVersion: "0.16.5"},
		{name: "a vanilla build has no loader at all", minecraft: "1.21.1"},
		{name: "everything absent", wantBad: ""},

		{name: "traversal in the minecraft version", minecraft: "../../packs/victim", loader: "fabric", wantBad: "minecraft"},
		{name: "a slash in the minecraft version", minecraft: "1.21.1/nested", wantBad: "minecraft"},
		{name: "a backslash in the loader", minecraft: "1.21.1", loader: `fab\ric`, wantBad: "loader"},
		{name: "traversal in the loader version", minecraft: "1.21.1", loader: "fabric", loaderVersion: "../..", wantBad: "loaderVersion"},
		{name: "a bare dot-dot", minecraft: "..", wantBad: "minecraft"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateBuildKeyComponents(tt.minecraft, tt.loader, tt.loaderVersion)
			if got != tt.wantBad {
				t.Errorf("validateBuildKeyComponents(%q, %q, %q) = %q, want %q",
					tt.minecraft, tt.loader, tt.loaderVersion, got, tt.wantBad)
			}
		})
	}
}

// The check has to sit on BOTH write paths. UpdateBuild writes the very same
// fields as CreateBuild, so guarding only the create leaves them settable one
// PATCH later.
func TestBothBuildWritePathsCheckTheKeyComponents(t *testing.T) {
	src := readPacksSource(t)
	if n := countOccurrences(src, "validateBuildKeyComponents(strings.TrimSpace(req.Minecraft)"); n != 2 {
		t.Errorf("the key-component check appears on %d build write paths, want 2 (CreateBuild and UpdateBuild)", n)
	}
}

func readPacksSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("packs.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	return string(raw)
}

func countOccurrences(s, sub string) int { return strings.Count(s, sub) }
