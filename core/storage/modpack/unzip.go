package modpack

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
)

// ReadZipEntry returns the bytes of the entry whose name equals innerPath (a
// content's TargetPath) inside a stored content zip, capped at maxBytes. ok is
// false when the zip is unreadable, has no such entry, or the entry exceeds
// maxBytes (checked against the declared uncompressed size first, so an
// oversized entry is never decompressed).
func ReadZipEntry(zipBytes []byte, innerPath string, maxBytes int64) (data []byte, ok bool) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, false
	}
	want := strings.ReplaceAll(innerPath, `\`, "/")
	for _, f := range zr.File {
		if strings.ReplaceAll(f.Name, `\`, "/") != want {
			continue
		}
		if f.UncompressedSize64 > uint64(maxBytes) {
			return nil, false
		}
		rc, err := f.Open()
		if err != nil {
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
		rc.Close()
		if err != nil || int64(len(b)) > maxBytes {
			return nil, false
		}
		return b, true
	}
	return nil, false
}

// IsUnsafeEntryPath reports whether a single archive entry name, after
// normalizing backslashes, is absolute (leading "/" or a Windows drive letter)
// or escapes the archive root via a ".." segment. Used to reject an entry name
// before it is written into an archive a node, a launcher or an operator
// extracts downstream.
func IsUnsafeEntryPath(name string) bool {
	name = strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(name, "/") {
		return true
	}
	// Windows drive-letter path (e.g. "C:\evil" -> "C:/evil") is absolute on a
	// Windows machine that extracts the pack; treat it as unsafe too.
	if len(name) >= 2 && name[1] == ':' &&
		((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) {
		return true
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
