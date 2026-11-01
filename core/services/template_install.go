package services

import (
	"errors"
	"fmt"
	"strings"

	"dylaris-core/models"
)

// TemplateInstallPlan is the install a template becomes: its files arrive as
// an upload-zip, and its software is installed over them with the fields the
// panel's online install sends for that software.
func TemplateInstallPlan(t models.Template) (installerType, software, version, mcVersion, loader string, err error) {
	sv := strings.TrimSpace(t.SoftwareVersion)
	mcVersion = strings.TrimSpace(t.McVersion)
	switch t.Software {
	case "":
		return "", "", "", "", "", errors.New("template has no server software set")
	case "neoforge":
		loader = sv
	case "fabric", "forge":
		version = mcVersion
		if version == "" {
			version = sv
		}
		// A template captured from an online install records the Minecraft
		// version as its software version (that install's build picker lists
		// Minecraft versions); the node then picks the loader, as it did there.
		if sv != version {
			loader = sv
		}
	case "paper", "vanilla":
		version = sv
		if version == "" {
			version = mcVersion
		}
	case "velocity", "waterfall", "bungeecord":
		version = sv
	default:
		return "", "", "", "", "", fmt.Errorf("template software %q cannot be installed", t.Software)
	}
	return "upload-zip", t.Software, version, mcVersion, loader, nil
}
