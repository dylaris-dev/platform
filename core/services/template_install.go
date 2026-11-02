package services

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-pkg/validate"
)

// TemplateSoftware is the software a template can install: exactly what the
// node installs over a template's files (uploadSoftware in node/installer.go).
// Purpur is not among them, the node has no installer for it.
var TemplateSoftware = []string{"paper", "vanilla", "fabric", "forge", "neoforge", "velocity", "waterfall", "bungeecord"}

// PaperMCReleasePattern is a stable Velocity or Waterfall release as the
// version list offers it ("3.5.1", "1.21"). Snapshots carry a "-" suffix and
// are not offered.
var PaperMCReleasePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

// BungeeCordBuildPattern is a BungeeCord Jenkins build, sent the way the
// version list writes it ("#2104"). The node accepts the same shape.
var BungeeCordBuildPattern = regexp.MustCompile(`^#?[0-9]{1,9}$`)

// InstallerVersionRefusal is the version rule of an install of typ, "" when
// it passes. The setup endpoint and every template save apply it, so a
// template is refused when it is saved, not when a server is made from it.
func InstallerVersionRefusal(typ, version, loader string) string {
	needsVersion := map[string]bool{"paper": true, "vanilla": true, "fabric": true, "forge": true, "velocity": true, "waterfall": true, "bungeecord": true}
	if needsVersion[typ] && strings.TrimSpace(version) == "" {
		return "installer.version is required for " + typ
	}
	switch typ {
	case "velocity", "waterfall":
		if !PaperMCReleasePattern.MatchString(version) {
			return "installer.version must be a " + typ + " release such as 3.5.1"
		}
	case "bungeecord":
		if !BungeeCordBuildPattern.MatchString(version) {
			return "installer.version must be a BungeeCord build number such as #2104"
		}
	case "neoforge":
		if strings.TrimSpace(loader) == "" {
			return "installer.loader is required for neoforge"
		}
	}
	return ""
}

var (
	// forgeFullVersion is Forge's own version name, "1.20.1-47.3.0", which is
	// also how Forge names its library folders.
	forgeFullVersion = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)+)-([0-9]+(?:\.[0-9]+)+)$`)
	forgeBuild       = regexp.MustCompile(`^[0-9]+(\.[0-9]+){2,3}$`)
	fabricLoader     = regexp.MustCompile(`^[0-9]+(\.[0-9]+){2}(\+build\.[0-9]+)?$`)
	neoForgeVersion  = regexp.MustCompile(`^[0-9]+(\.[0-9]+){2,3}(-[0-9A-Za-z.]+)?$`)
)

// NormalizeTemplateSoftware returns t with its software lower-cased and its
// versions in the one form the install reads, or why it cannot be installed.
// Empty software passes: such a template is files only until one is set.
//
//   - paper, vanilla: the Minecraft version decides, the node installs its
//     newest build. The software version is dropped; it fills an empty
//     Minecraft version only when it is one, so a build number never goes out
//     as a Minecraft version.
//   - forge: the build ("47.3.0"); Forge's full name "1.20.1-47.3.0" is split.
//   - fabric: the loader ("0.16.10").
//   - forge, fabric: a software version equal to the Minecraft version (what a
//     capture recorded before it pinned the loader) means "the newest".
//   - neoforge: the NeoForge version ("21.1.265"), required.
func NormalizeTemplateSoftware(t models.Template) (models.Template, error) {
	sw := strings.ToLower(strings.TrimSpace(t.Software))
	mc := strings.TrimSpace(t.McVersion)
	sv := strings.TrimSpace(t.SoftwareVersion)
	t.Software, t.McVersion, t.SoftwareVersion = sw, mc, sv
	if sw == "" {
		return t, nil
	}
	if !slices.Contains(TemplateSoftware, sw) {
		if sw == "purpur" {
			return t, errors.New("purpur is not supported; use paper, which runs the same plugins")
		}
		return t, fmt.Errorf("software %q is not supported; use one of %s", sw, strings.Join(TemplateSoftware, ", "))
	}
	switch sw {
	case "paper", "vanilla":
		if mc == "" {
			if !validate.McVersion.MatchString(sv) {
				return t, fmt.Errorf("a %s template needs the Minecraft version, such as 1.21.4 (the newest %s build of it is installed)", sw, sw)
			}
			t.McVersion = sv
		}
		t.SoftwareVersion = ""
	case "forge", "fabric":
		if m := forgeFullVersion.FindStringSubmatch(sv); sw == "forge" && m != nil {
			if mc != "" && mc != m[1] {
				return t, fmt.Errorf("forge %s is for Minecraft %s, not %s", sv, m[1], mc)
			}
			mc, sv = m[1], m[2]
		}
		if mc == "" && strings.HasPrefix(sv, "1.") {
			mc = sv
		}
		if mc == "" {
			return t, fmt.Errorf("a %s template needs the Minecraft version, such as 1.21.4", sw)
		}
		if sv != "" && sv != mc {
			if sw == "forge" && (!forgeBuild.MatchString(sv) || strings.HasPrefix(sv, "1.")) {
				return t, fmt.Errorf("%q is not a Forge build; builds look like 47.3.0, or leave it empty for the recommended one", sv)
			}
			if sw == "fabric" && !fabricLoader.MatchString(sv) {
				return t, fmt.Errorf("%q is not a Fabric loader version; they look like 0.16.10, or leave it empty for the newest", sv)
			}
		}
		t.McVersion, t.SoftwareVersion = mc, sv
	case "neoforge":
		if sv == "" {
			return t, errors.New("a neoforge template needs the NeoForge version, such as 21.1.265 for Minecraft 1.21.1")
		}
		// From 26 on a NeoForge version starts with the Minecraft version and has
		// four parts ("26.1.0.5"); before, it never starts with "1.".
		base := strings.SplitN(sv, "-", 2)[0]
		if !neoForgeVersion.MatchString(sv) || strings.HasPrefix(sv, "1.") || (leadingNumber(sv) >= 26 && strings.Count(base, ".") < 3) {
			return t, fmt.Errorf("%q is a Minecraft version, not a NeoForge version; NeoForge versions look like 21.1.265 (for Minecraft 1.21.1)", sv)
		}
		if mc == "" {
			t.McVersion = NeoForgeMinecraft(sv)
		}
	}
	return t, nil
}

func leadingNumber(v string) int {
	n, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	return n
}

// ValidateTemplateSoftware normalises t's software and versions in place and
// refuses what its install would refuse. Every template write calls it.
func ValidateTemplateSoftware(t *models.Template) error {
	n, err := NormalizeTemplateSoftware(*t)
	if err != nil {
		return err
	}
	if n.Software != "" {
		_, sw, v, _, loader, err := TemplateInstallPlan(n)
		if err != nil {
			return err
		}
		if msg := InstallerVersionRefusal(sw, v, loader); msg != "" {
			return errors.New(msg)
		}
	}
	t.Software, t.McVersion, t.SoftwareVersion = n.Software, n.McVersion, n.SoftwareVersion
	return nil
}

// TemplateInstallPlan is the install a template becomes: its files arrive as
// an upload-zip, and its software is installed over them with the fields the
// panel's online install sends for that software. A template saved before
// NormalizeTemplateSoftware existed is normalised here the same way.
func TemplateInstallPlan(t models.Template) (installerType, software, version, mcVersion, loader string, err error) {
	if strings.TrimSpace(t.Software) == "" {
		return "", "", "", "", "", errors.New("template has no server software set")
	}
	if t, err = NormalizeTemplateSoftware(t); err != nil {
		return "", "", "", "", "", fmt.Errorf("template cannot be installed: %w", err)
	}
	sv, mcVersion := t.SoftwareVersion, t.McVersion
	switch t.Software {
	case "neoforge":
		loader = sv
	case "fabric", "forge":
		version = mcVersion
		if sv != version {
			loader = sv
		}
	case "paper", "vanilla":
		version = mcVersion
	case "velocity", "waterfall", "bungeecord":
		version = sv
	}
	return "upload-zip", t.Software, version, mcVersion, loader, nil
}

// TemplateLoaderLibraryDir is where an installed sub-server keeps the
// libraries of software's loader, one folder per installed version; "" for
// software without one.
func TemplateLoaderLibraryDir(software string) string {
	switch software {
	case "forge":
		return "libraries/net/minecraftforge/forge"
	case "neoforge":
		return "libraries/net/neoforged/neoforge"
	case "fabric":
		return "libraries/net/fabricmc/fabric-loader"
	}
	return ""
}

// TemplateLoaderFromLibraries is the loader version a sub-server runs, read
// from the folder names in its TemplateLoaderLibraryDir (Forge names them
// "<mc>-<build>"). "" unless there is exactly one candidate: a tree with an
// older loader left over pins nothing rather than possibly the wrong one.
func TemplateLoaderFromLibraries(software, mcVersion string, dirs []string) string {
	found := ""
	for _, d := range dirs {
		v := d
		if software == "forge" {
			m := forgeFullVersion.FindStringSubmatch(d)
			if m == nil || (mcVersion != "" && m[1] != mcVersion) {
				continue
			}
			v = m[2]
		}
		if found != "" {
			return ""
		}
		found = v
	}
	return found
}

// templatePrivacyLog is a log file name: "latest.log", "proxy.log.0", "x.log.gz".
var templatePrivacyLog = regexp.MustCompile(`\.log(\.[0-9]+)?(\.gz)?$`)

// TemplatePrivacyPaths are the selected capture paths that can hold player
// names or IP addresses (logs, crash reports, the name cache), for a warning:
// a template can be shared.
func TemplatePrivacyPaths(selected []string) []string {
	var out []string
	for _, p := range selected {
		base := path.Base(p)
		hit := templatePrivacyLog.MatchString(base) || base == "usercache.json"
		for _, s := range strings.Split(p, "/") {
			hit = hit || s == "logs" || s == "crash-reports"
		}
		if hit {
			out = append(out, p)
		}
	}
	return out
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
