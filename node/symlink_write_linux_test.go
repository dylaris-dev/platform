package main

import (
	"os"
	"path/filepath"
	"testing"

	pb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"

	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
)

// plantLinks lays out a server the way a tenant's own plugin can: a node-owned
// file in the server root, and links to it from inside a sub-server directory,
// which the container can write.
func plantLinks(t *testing.T, serverDir string) string {
	t.Helper()
	cfg := filepath.Join(serverDir, ".node_config.json")
	for _, p := range []string{serverDir, filepath.Join(serverDir, "survival"), filepath.Join(serverDir, ".dylaris-backups")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg, []byte(`{"ram":2048}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.node_config.json", filepath.Join(serverDir, "survival", "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.dylaris-backups", filepath.Join(serverDir, "survival", "d")); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func assertUntouched(t *testing.T, serverDir, cfg string) {
	t.Helper()
	if got, _ := os.ReadFile(cfg); string(got) != `{"ram":2048}` {
		t.Fatalf(".node_config.json was rewritten through a link: %q", got)
	}
	if ents, _ := os.ReadDir(filepath.Join(serverDir, ".dylaris-backups")); len(ents) != 0 {
		t.Fatalf("something was written into .dylaris-backups through a link: %v", ents)
	}
}

// Every name check read the path's text, and os.Root follows a link that
// stays inside the server: a save to survival/x wrote the node's own config,
// the one the container is rebuilt from. Beam.
func TestBeamWritesDoNotFollowATenantLink(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	cfg := plantLinks(t, dir)

	bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/x", Content: `{"ram":999999}`})
	bs.CreateFile(ctx, &pb.BeamFileCreateReq{Path: "survival/x"})
	bs.CreateFile(ctx, &pb.BeamFileCreateReq{Path: "survival/d/planted.tar.gz"})
	bs.RenameFile(ctx, &pb.BeamFileRenameReq{OldPath: "survival/x", NewName: "y"})
	assertUntouched(t, dir, cfg)
}

// The same, through the panel's file API.
func TestPanelWritesDoNotFollowATenantLink(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := NewStreamHandler(sm)
	const uuid = "22222222-2222-2222-2222-222222222222"
	dir := sm.GetServerDir(uuid)
	cfg := plantLinks(t, dir)

	h.handleCreate("r1", uuid, &nodepb.CreateFileReq{Path: "survival/x"})
	h.handleCreate("r2", uuid, &nodepb.CreateFileReq{Path: "survival/d/planted"})
	if f, tmp, err := h.createUploadTemp(uuid, "survival/d/planted.tar.gz"); err == nil {
		f.Write([]byte("x"))
		f.Close()
		h.commitUpload(uuid, "survival/d/planted.tar.gz", tmp)
	}
	assertUntouched(t, dir, cfg)
}

// And over SFTP.
func TestSFTPWritesDoNotFollowATenantLink(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	const uuid = "33333333-3333-3333-3333-333333333333"
	dir := sm.GetServerDir(uuid)
	cfg := plantLinks(t, dir)
	fs := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, sm, nil, "u")

	if w, err := fs.Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/x", Flags: 0x02 | 0x08 | 0x10}); err == nil {
		w.WriteAt([]byte("{}"), 0)
		w.(interface{ Close() error }).Close()
	}
	fs.Filecmd(&sftp.Request{Method: "Mkdir", Filepath: "s/survival/d/planted"})
	assertUntouched(t, dir, cfg)
}

// A mod install checked the path once and then downloaded for seconds, every
// os call re-resolving it: the tenant could swap mods/ for a link to anywhere
// on the host in between. The directory is now opened once without following
// a link, and a link where it should be is refused.
func TestPinDirRefusesALinkedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(root, "survival"), 0o755)
	if err := os.Symlink(outside, filepath.Join(root, "survival", "mods")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pinDir(root, "survival/mods", true); err == nil {
		t.Fatal("a linked mods directory was pinned")
	}
	dir, release, err := pinDir(root, "survival/plugins", true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.WriteFile(filepath.Join(dir, "a.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "survival", "plugins", "a.jar")); err != nil {
		t.Fatalf("a write through the pinned path did not land in the directory: %v", err)
	}
}
