package packs

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage/modpack"
)

func validContentSide(side string) bool {
	return side == models.SideClient || side == models.SideServer || side == models.SideBoth
}

func validContentType(kind string) bool {
	switch kind {
	case models.ContentTypeMod, models.ContentTypeResourcepack, models.ContentTypeShaderpack, models.ContentTypeConfig, models.ContentTypeOther:
		return true
	}
	return false
}

// A content path is interpreted by launchers on both Windows and Linux.
func validContentTarget(target string) bool {
	if target == "" || len(target) > 512 || modpack.IsUnsafeEntryPath(target) || strings.ContainsAny(target, `\:`) || strings.HasSuffix(target, "/") {
		return false
	}
	for _, r := range target {
		if unicode.IsControl(r) {
			return false
		}
	}
	return path.Clean(target) == target && target != "."
}

func validateModrinthContent(build *models.PackBuild, v *services.ModrinthVersion, kind string) error {
	if kind != models.ContentTypeMod && kind != models.ContentTypeResourcepack && kind != models.ContentTypeShaderpack {
		return fmt.Errorf("Modrinth content must be a mod, resource pack or shader pack")
	}
	file := v.PrimaryFile()
	if v.ID == "" || v.ProjectID == "" || !strings.HasPrefix(file.URL, "https://cdn.modrinth.com/") || file.Size < 0 || file.Hashes["sha1"] == "" || file.Hashes["sha512"] == "" || !validContentTarget(file.Filename) || strings.Contains(file.Filename, "/") {
		return fmt.Errorf("Modrinth version has no valid downloadable artifact")
	}
	if kind == models.ContentTypeMod {
		if ok, reason := services.ModVersionFits(build.Loader, build.Minecraft, v.Loaders, v.GameVersions); !ok {
			return fmt.Errorf("content does not match this pack build: %s", reason)
		}
		return nil
	}
	// Resource/shader packs use their own loader tags, not the mod loader.
	for _, mc := range v.GameVersions {
		if mc == build.Minecraft && mc != "" {
			return nil
		}
	}
	return fmt.Errorf("content does not support this pack's Minecraft version")
}
