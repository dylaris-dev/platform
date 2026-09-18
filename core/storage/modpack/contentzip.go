package modpack

import (
	"archive/zip"
	"bytes"
	"fmt"
	"time"
)

// zeroModTime is a fixed timestamp stamped into every content-zip entry so the
// archive bytes are reproducible run to run: storing the same file twice yields
// the same object and the same hashes, where the default archive/zip behaviour
// (the current time) would make every re-store look like a change.
var zeroModTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// BuildContentZip packages a single file into a content zip whose one entry
// sits at innerPath (a .minecraft-relative path such as "mods/foo.jar" or
// "resourcepacks/bar.zip"). This is how the pack builder stores what it is
// given: the .mrpack and server-pack renders copy the entry out into the
// instance root, so innerPath must already be instance-relative. The entry's
// mod-time is zeroed for byte-stable, reproducible output.
//
// An innerPath that escapes the instance root is refused here rather than at
// each call site. The renders copy these verbatim into a pack that a node or a
// launcher extracts, so this is the one place every content zip we author
// passes through, and one of the callers -
// the pack text editor - re-wraps a stored zip around a target path read back
// out of the database, skipping the validation the original upload went
// through.
func BuildContentZip(innerPath string, content []byte) ([]byte, error) {
	if innerPath == "" {
		return nil, fmt.Errorf("contentzip: empty inner path")
	}
	if IsUnsafeEntryPath(innerPath) {
		return nil, fmt.Errorf("contentzip: unsafe inner path %q", innerPath)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: innerPath, Method: zip.Deflate}
	hdr.Modified = zeroModTime
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		_ = zw.Close()
		return nil, err
	}
	if _, err := w.Write(content); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
