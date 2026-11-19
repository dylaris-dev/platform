package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// R32-08: the hard-link write guard knew only three of the node's files at the
// server top, so a container that linked .dylaris_live into a tenant folder
// had a member's SFTP or Beam write replace the move marker through it.
func TestATenantWriteRefusesALinkToAMoveMarker(t *testing.T) {
	for _, name := range []string{storageLiveMarker, storageMovedAwayMarker, netGateFile, metadataTempName} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("100"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "survival"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(dir, name), filepath.Join(dir, "survival", "x")); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			f, err := openTenantWriteIn(root, "survival/x", os.O_WRONLY|os.O_TRUNC, 0o644, dir)
			if err == nil {
				f.Close()
			}
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("openTenantWriteIn = %v, want a refusal", err)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != "100" {
				t.Fatalf("%s now holds %q", name, b)
			}
		})
	}
}
