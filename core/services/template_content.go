package services

import (
	"archive/zip"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"path"
	"strings"

	"dylaris-core/models"
)

const (
	templateJarReason = "jar files are not allowed in templates on this platform"
	// templateJarPlaceReason is for a jar under no-jars that could have been a
	// reference but sits where an install cannot lay one.
	templateJarPlaceReason = "only jars in mods/ or plugins/ can be kept as Modrinth references"
	templateZipReason      = "archives inside a template may not contain jar or class files"
	// templateUploadStaging is the temp name uploads are staged under; a
	// template path may never collide with it.
	templateUploadStaging = ".upload.zip"
	// maxTemplatePathDepth stays under the 64 levels storage.WalkProvider
	// walks, so a file can never be written where listing, size and delete
	// cannot reach it.
	maxTemplatePathDepth = 48
)

// TemplateRelPath cleans raw into a forward-slash relative path. It refuses
// anything that could leave the template root: empty, absolute, drive letters,
// backslashes, NUL, any ".." segment (checked before cleaning, so "a/../x" is
// refused rather than silently rewritten), the upload staging name, and paths
// deeper than maxTemplatePathDepth segments.
func TemplateRelPath(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "\\\x00") || strings.HasPrefix(raw, "/") {
		return "", false
	}
	if len(raw) >= 2 && raw[1] == ':' {
		return "", false
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." || strings.HasPrefix(seg, templateUploadStaging) {
			return "", false
		}
	}
	clean := path.Clean(raw)
	if clean == "." || strings.Count(clean, "/") >= maxTemplatePathDepth {
		return "", false
	}
	return clean, true
}

// TemplateFileAllowed applies the content policy to one template path.
func TemplateFileAllowed(rel, policy string) (bool, string) {
	if policy == TemplatePolicyNoJars && strings.HasSuffix(strings.ToLower(rel), ".jar") {
		if strings.Contains(rel, "/") && !templateJarInModsOrPlugins(rel) {
			return false, templateJarPlaceReason
		}
		return false, templateJarReason
	}
	return true, ""
}

// TemplateZipMagic reports whether head starts like a zip, which a jar is: a
// renamed jar is caught by its bytes, not its name. It catches plain renames
// only; deliberate obfuscation (a prepended byte, a jar found by its central
// directory) is a terms violation, not something a filename or format check
// can stop.
func TemplateZipMagic(head []byte) bool {
	return len(head) >= 4 && string(head[:4]) == "PK\x03\x04"
}

// TemplateZipAllowed inspects an uploaded nested archive. Under no-jars it is
// refused when any entry is a jar, a class file (a jar is a zip of classes) or
// itself a zip (one level stops the trivial bypass; deeper nesting is not
// unpacked). An unreadable archive cannot be inspected and is refused too.
func TemplateZipAllowed(r io.ReaderAt, size int64, policy string) (bool, string) {
	if policy != TemplatePolicyNoJars {
		return true, ""
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return false, templateZipReason
	}
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		if strings.HasSuffix(n, ".jar") || strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".class") {
			return false, templateZipReason
		}
	}
	return true, ""
}

const (
	TemplateJarNotOnModrinth = "not on Modrinth, and jar files are not stored in templates on this platform"
	TemplateJarUnreachable   = "Modrinth unreachable, try again"
)

// TemplateJarReferable reports whether rel is a jar that, under no-jars, is
// looked up at Modrinth and kept as a reference. A root-level jar is the
// server software, which an install lays fresh, so it is never referenced.
// Only under mods/ or plugins/: that is where an install lays references
// (TemplateModrinthFiles) and where the node accepts them.
func TemplateJarReferable(rel, policy string) bool {
	return policy == TemplatePolicyNoJars && strings.HasSuffix(strings.ToLower(rel), ".jar") && templateJarInModsOrPlugins(rel)
}

func templateJarInModsOrPlugins(rel string) bool {
	return strings.HasPrefix(rel, "mods/") || strings.HasPrefix(rel, "plugins/")
}

// TemplateJar is a jar offered to a template, hashed but not stored.
type TemplateJar struct {
	Path   string
	Sha1   string
	Sha512 string
	Size   int64
}

// HashTemplateJar streams r once and returns its hashes and length.
func HashTemplateJar(rel string, r io.Reader) (TemplateJar, error) {
	h1, h512 := sha1.New(), sha512.New()
	n, err := io.Copy(io.MultiWriter(h1, h512), r)
	if err != nil {
		return TemplateJar{}, err
	}
	return TemplateJar{Path: rel, Sha1: hex.EncodeToString(h1.Sum(nil)), Sha512: hex.EncodeToString(h512.Sum(nil)), Size: n}, nil
}

// ResolveTemplateJars looks every jar up at Modrinth in one batched lookup.
// A jar becomes a reference only when a file of the version Modrinth names has
// exactly its sha1 and sha512 and is served from the Modrinth CDN; every other
// jar gets a skip reason. When Modrinth does not answer, every jar is skipped
// as unreachable: "not on Modrinth" would be a false claim.
func ResolveTemplateJars(jars []TemplateJar) ([]models.TemplateModrinthRef, map[string]string) {
	reasons := map[string]string{}
	if len(jars) == 0 {
		return nil, reasons
	}
	sha1s := make([]string, 0, len(jars))
	for _, j := range jars {
		sha1s = append(sha1s, j.Sha1)
	}
	known, err := ModrinthByHashes(sha1s)
	var refs []models.TemplateModrinthRef
	for _, j := range jars {
		if err != nil {
			reasons[j.Path] = TemplateJarUnreachable
			continue
		}
		ref, ok := templateJarRef(j, known[j.Sha1])
		if !ok {
			reasons[j.Path] = TemplateJarNotOnModrinth
			continue
		}
		refs = append(refs, ref)
	}
	return refs, reasons
}

func templateJarRef(j TemplateJar, v ModrinthVersion) (models.TemplateModrinthRef, bool) {
	if v.ID == "" || v.ProjectID == "" {
		return models.TemplateModrinthRef{}, false
	}
	for _, f := range v.Files {
		if !strings.EqualFold(f.Hashes["sha1"], j.Sha1) || !strings.EqualFold(f.Hashes["sha512"], j.Sha512) ||
			!strings.HasPrefix(f.URL, modrinthCDNPrefix) {
			continue
		}
		return models.TemplateModrinthRef{
			Path: j.Path, ProjectID: v.ProjectID, VersionID: v.ID, FileName: f.Filename,
			URL: f.URL, Sha1: j.Sha1, Sha512: j.Sha512, SizeBytes: j.Size,
		}, true
	}
	return models.TemplateModrinthRef{}, false
}
