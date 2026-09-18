package modpack

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
)

// WrapJarAsContentZip packages a single mod jar into a content zip whose
// contents extract into the .minecraft root: the jar lands at mods/<fileName>.
// The pack renders copy that entry out as-is.
func WrapJarAsContentZip(fileName string, jar []byte) ([]byte, error) {
	if fileName == "" {
		return nil, fmt.Errorf("wrap: empty file name")
	}
	// fileName is a base name, not a path: it is concatenated onto "mods/"
	// below, so a "../" in it lands the jar outside the instance when the
	// pack is extracted. Both callers derive it from a name they sanitized
	// first; refusing it here is what keeps that true for the next caller.
	if IsUnsafeEntryPath("mods/" + fileName) {
		return nil, fmt.Errorf("wrap: unsafe file name %q", fileName)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("mods/" + fileName)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(jar); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Hashes returns hex md5, sha1 and sha512 over the bytes; Modrinth matches a
// file by the last two.
func Hashes(data []byte) (md5hex, sha1hex, sha512hex string) {
	m := md5.Sum(data)
	s1 := sha1.Sum(data)
	s5 := sha512.Sum512(data)
	return hex.EncodeToString(m[:]), hex.EncodeToString(s1[:]), hex.EncodeToString(s5[:])
}
