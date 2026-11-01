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

// MaxTemplateModrinthRefs caps the Modrinth references of one template: each
// is a download on every install, and a real modpack has a few hundred at
// most. The node refuses a longer list too (templateModrinthMax).
const MaxTemplateModrinthRefs = 500

// TemplateModrinthFile is one Modrinth reference as the node downloads it on
// install. Path is template-relative; the node lays it under the sub-server
// root, like the files of the template's zip.
type TemplateModrinthFile struct {
	Path   string `json:"path"`
	URL    string `json:"url"`
	Sha512 string `json:"sha512"`
	Size   int64  `json:"size"`
}

// TemplateModrinthFiles turns a template's references into the install
// command's modrinthFiles. The node checks every field again; refusing here
// keeps an install that would fail on the node from being dispatched at all.
func TemplateModrinthFiles(refs []models.TemplateModrinthRef) ([]TemplateModrinthFile, error) {
	if len(refs) > MaxTemplateModrinthRefs {
		return nil, fmt.Errorf("the template has %d Modrinth references, more than the %d one template may hold; remove some first", len(refs), MaxTemplateModrinthRefs)
	}
	out := make([]TemplateModrinthFile, 0, len(refs))
	for _, r := range refs {
		dir, _, ok := templateModrinthPlace(r.Path)
		if !ok || dir == "" {
			return nil, fmt.Errorf("the template references %q, which is not a jar under mods/ or plugins/; remove it from the template", r.Path)
		}
		if !strings.HasPrefix(r.URL, modrinthCDNPrefix) || len(r.Sha512) != 128 {
			return nil, fmt.Errorf("the template reference %q has no Modrinth download with a sha512; remove it and add it again", r.Path)
		}
		out = append(out, TemplateModrinthFile{Path: r.Path, URL: r.URL, Sha512: r.Sha512, Size: r.SizeBytes})
	}
	return out, nil
}

// templateModrinthPlace splits a reference path into its top directory (mods
// or plugins, else "") and file name. ok is false for a path that is not a
// clean template path ending in .jar.
func templateModrinthPlace(p string) (top, name string, ok bool) {
	clean, ok := TemplateRelPath(p)
	if !ok || clean != p || !strings.HasSuffix(strings.ToLower(clean), ".jar") {
		return "", "", false
	}
	segs := strings.Split(clean, "/")
	if len(segs) < 2 || (segs[0] != "mods" && segs[0] != "plugins") {
		return "", segs[len(segs)-1], true
	}
	return segs[0], segs[len(segs)-1], true
}

// TemplateModrinthModRows are the server_mods rows a template install writes
// for its references, so the content page and the update checker know them.
// Only a jar directly in mods/ or plugins/ gets one: a row names its jar by
// target dir and file name, and a jar one folder deeper would be a row whose
// uninstall removes nothing.
func TemplateModrinthModRows(serverID int, subServer, installedBy string, refs []models.TemplateModrinthRef) []models.ServerMod {
	var rows []models.ServerMod
	for _, r := range refs {
		top, name, ok := templateModrinthPlace(r.Path)
		if !ok || top == "" || r.Path != top+"/"+name || r.ProjectID == "" {
			continue
		}
		row := models.ServerMod{
			ServerID:          serverID,
			SubServerName:     subServer,
			ModrinthProjectID: r.ProjectID,
			ModrinthVersionID: r.VersionID,
			FileName:          name,
			TargetDir:         top,
			SHA512:            r.Sha512,
			Status:            models.ServerModInstalled,
		}
		if installedBy != "" {
			by := installedBy
			row.InstalledBy = &by
		}
		rows = append(rows, row)
	}
	return rows
}
