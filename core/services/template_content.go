package services

import (
	"archive/zip"
	"io"
	"path"
	"strings"
)

const (
	templateJarReason = "jar files are not allowed in templates on this platform"
	templateZipReason = "archives inside a template may not contain jar files"
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
		return false, templateJarReason
	}
	return true, ""
}

// TemplateZipAllowed inspects an uploaded nested archive. Under no-jars it is
// refused when any entry is a jar or itself a zip (one level stops the trivial
// bypass; deeper nesting is not unpacked). An unreadable archive cannot be
// inspected and is refused too.
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
		if strings.HasSuffix(n, ".jar") || strings.HasSuffix(n, ".zip") {
			return false, templateZipReason
		}
	}
	return true, ""
}
