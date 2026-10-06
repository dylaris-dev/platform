package modpack

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
)

// WrapJarAsContentZip packages a single mod jar into a content zip whose
// contents extract into the .minecraft root: the jar lands at mods/<fileName>.
// The pack renders copy that entry out as-is.
func WrapJarAsContentZip(fileName string, jar []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteJarContentZip(&buf, fileName, bytes.NewReader(jar)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteJarContentZip is WrapJarAsContentZip writing to w as the jar is read, so
// a large upload is wrapped without holding the jar or the zip in memory. Both
// produce the same bytes, which keeps the stored hash (and so the storage key)
// of a re-uploaded jar unchanged.
func WriteJarContentZip(w io.Writer, fileName string, jar io.Reader) error {
	if fileName == "" {
		return fmt.Errorf("wrap: empty file name")
	}
	// fileName is a base name, not a path: it is concatenated onto "mods/"
	// below, so a "../" in it lands the jar outside the instance when the
	// pack is extracted. Both callers derive it from a name they sanitized
	// first; refusing it here is what keeps that true for the next caller.
	if IsUnsafeEntryPath("mods/" + fileName) {
		return fmt.Errorf("wrap: unsafe file name %q", fileName)
	}
	zw := zip.NewWriter(w)
	ew, err := zw.Create("mods/" + fileName)
	if err != nil {
		return err
	}
	if _, err := io.Copy(ew, jar); err != nil {
		return err
	}
	return zw.Close()
}

// Hashes returns hex md5, sha1 and sha512 over the bytes; Modrinth matches a
// file by the last two.
func Hashes(data []byte) (md5hex, sha1hex, sha512hex string) {
	m := md5.Sum(data)
	s1 := sha1.Sum(data)
	s5 := sha512.Sum512(data)
	return hex.EncodeToString(m[:]), hex.EncodeToString(s1[:]), hex.EncodeToString(s5[:])
}
