package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/pkg/sftp"
)

// J32/J34: on a host without fs.protected_hardlinks a plugin links the node's
// own .node_config.json into a tenant folder, and a write to that name went
// into the node's file. A link between two of the tenant's own files is still
// written.
func TestATenantWriteRefusesALinkToANodeFile(t *testing.T) {
	const config = `{"image":"node"}`
	setup := func(t *testing.T) (*StorageManager, string) {
		sm, paths := newMovableServer(t)
		dir := filepath.Join(paths[0], movingUUID)
		if err := os.WriteFile(filepath.Join(dir, ".node_config.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(dir, ".node_config.json"), filepath.Join(dir, "survival", "x")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "survival", "a"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(dir, "survival", "a"), filepath.Join(dir, "survival", "b")); err != nil {
			t.Fatal(err)
		}
		return sm, dir
	}
	refs := []sftpServerRef{{UUID: movingUUID, Name: "s", Perms: fileperms.Full()}}
	const write, creat, trunc = 0x02, 0x08, 0x10

	for _, tc := range []struct {
		name  string
		write func(t *testing.T, sm *StorageManager, rel string) error
	}{
		{"sftp put", func(t *testing.T, sm *StorageManager, rel string) error {
			w, err := newVirtualFS(refs, sm, nil, "ivy").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + rel, Flags: write | creat | trunc})
			if err != nil {
				return err
			}
			if _, err := w.WriteAt([]byte("tenant"), 0); err != nil {
				return err
			}
			return w.(io.Closer).Close()
		}},
		{"sftp truncate", func(t *testing.T, sm *StorageManager, rel string) error {
			r := &sftp.Request{Method: "Setstat", Filepath: "s/" + rel, Flags: 0x1, Attrs: []byte{0, 0, 0, 0, 0, 0, 0, 0}}
			return newVirtualFS(refs, sm, nil, "ivy").Filecmd(r)
		}},
		{"beam save", func(t *testing.T, sm *StorageManager, rel string) error {
			bs, _, ctx := newTestBeamServer(t)
			bs.storageMgr = sm
			bs.serverUUIDByPeer.Range(func(k, _ any) bool { bs.serverUUIDByPeer.Store(k, movingUUID); return true })
			resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: rel, Content: "tenant"})
			if err != nil {
				return err
			}
			if !resp.Success {
				return os.ErrPermission
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm, dir := setup(t)
			if err := tc.write(t, sm, "survival/x"); err == nil {
				t.Error("a write through a link to .node_config.json was taken")
			}
			if b, _ := os.ReadFile(filepath.Join(dir, ".node_config.json")); string(b) != config {
				t.Errorf(".node_config.json now holds %q", b)
			}
			if err := tc.write(t, sm, "survival/b"); err != nil {
				t.Errorf("a write to a tenant file with two names: %v", err)
			}
		})
	}
}
