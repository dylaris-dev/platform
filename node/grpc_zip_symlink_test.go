package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	pb "dylaris-proto/node"
)

// collectZipFromStream drains the NodeMessage stream a zip helper produces and
// returns the assembled archive.
func collectZipFromStream(t *testing.T, run func(sendFn func(*pb.NodeMessage) error)) []byte {
	t.Helper()
	var buf bytes.Buffer
	run(func(m *pb.NodeMessage) error {
		if c := m.GetChunk(); c != nil {
			buf.Write(c.Data)
		}
		if e := m.GetError(); e != nil {
			t.Fatalf("stream error: %s", e.Message)
		}
		return nil
	})
	return buf.Bytes()
}

func zipNamesAndBodies(t *testing.T, raw []byte) (map[string]bool, map[string]string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("not a zip (%d bytes): %v", len(raw), err)
	}
	names := map[string]bool{}
	bodies := map[string]string{}
	for _, f := range zr.File {
		names[f.Name] = true
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		var sb bytes.Buffer
		sb.ReadFrom(rc)
		rc.Close()
		bodies[f.Name] = sb.String()
	}
	return names, bodies
}

// seedControlPlaneTree lays out a server dir for the StreamHandler and plants
// one escaping and one contained symlink. Returns the handler, uuid and the
// path of the secret that must never appear in an archive.
func seedControlPlaneTree(t *testing.T) (*StreamHandler, string, string) {
	t.Helper()
	sm := NewStorageManager(t.TempDir(), nil)
	h := NewStreamHandler(sm)
	const uuid = "22222222-2222-2222-2222-222222222222"
	root := sm.GetServerDir(uuid)

	if err := os.MkdirAll(filepath.Join(root, "plugins"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server.properties"), []byte("motd=hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "ok.jar"), []byte("JAR"), 0644); err != nil {
		t.Fatal(err)
	}

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "plugins", "escape.txt")); err != nil {
		if runtime.GOOS == "windows" || errors.Is(err, os.ErrPermission) {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatal(err)
	}
	// Relative, because a Root refuses absolute links even when they point
	// inside (see rootfs.go); a relative link is the contained form.
	if err := os.Symlink(filepath.Join("..", "server.properties"), filepath.Join(root, "plugins", "inside.txt")); err != nil {
		t.Fatal(err)
	}
	return h, uuid, secret
}

// TestControlPlaneZip_SymlinkEscapingRootIsOmitted covers the SAME hole as the
// Beam test, on the older control-plane path that Core's HTTP file browser and
// the backup builder use. filepath.Walk reports links via Lstat but os.Open
// follows them, so an unguarded walk would archive whatever the link points at.
func TestControlPlaneZip_SymlinkEscapingRootIsOmitted(t *testing.T) {
	t.Run("streamDirAsZip", func(t *testing.T) {
		h, uuid, _ := seedControlPlaneTree(t)
		root, err := os.OpenRoot(h.serverDir(uuid))
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		raw := collectZipFromStream(t, func(send func(*pb.NodeMessage) error) {
			h.streamDirAsZip("r1", root, "plugins", "plugins.zip", send)
		})
		names, bodies := zipNamesAndBodies(t, raw)
		if names["escape.txt"] {
			t.Error("escaping symlink was archived")
		}
		for n, b := range bodies {
			if b == "TOP SECRET" {
				t.Errorf("content from outside the server dir leaked as %q", n)
			}
		}
		if bodies["inside.txt"] != "motd=hi" {
			t.Errorf("contained symlink dropped: inside.txt = %q", bodies["inside.txt"])
		}
	})

	// The selected entry is itself the escaping link, which is the case os.Stat
	// would have followed before any containment check ran.
	t.Run("streamSelectiveZip with the link selected directly", func(t *testing.T) {
		h, uuid, _ := seedControlPlaneTree(t)
		raw := collectZipFromStream(t, func(send func(*pb.NodeMessage) error) {
			h.streamSelectiveZip("r2", uuid, &pb.SelectiveReadReq{
				BasePath: "plugins",
				Selected: []string{"escape.txt", "ok.jar"},
			}, send)
		})
		names, bodies := zipNamesAndBodies(t, raw)
		if names["escape.txt"] {
			t.Error("escaping symlink was archived when selected directly")
		}
		for n, b := range bodies {
			if b == "TOP SECRET" {
				t.Errorf("content from outside the server dir leaked as %q", n)
			}
		}
		// Dropping the bad entry must not take the good one with it.
		if bodies["ok.jar"] != "JAR" {
			t.Errorf("ok.jar = %q, want JAR", bodies["ok.jar"])
		}
	})

	t.Run("streamSelectiveZip walking a folder that contains the link", func(t *testing.T) {
		h, uuid, _ := seedControlPlaneTree(t)
		raw := collectZipFromStream(t, func(send func(*pb.NodeMessage) error) {
			h.streamSelectiveZip("r3", uuid, &pb.SelectiveReadReq{
				BasePath: "",
				Selected: []string{"plugins"},
			}, send)
		})
		names, bodies := zipNamesAndBodies(t, raw)
		if names["plugins/escape.txt"] {
			t.Error("escaping symlink was archived via the folder walk")
		}
		for n, b := range bodies {
			if b == "TOP SECRET" {
				t.Errorf("content from outside the server dir leaked as %q", n)
			}
		}
	})
}
