package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"

	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
)

// A folder copied INTO an existing sub-server was written through a root at
// the server directory, which holds .node_config.json; a link the running
// server planted inside the sub-server was followed out to it. No race: the
// link can sit there in advance.
func TestCopyIntoASubServerDoesNotFollowItsLinks(t *testing.T) {
	plantPayload := func(t *testing.T, dir string) {
		t.Helper()
		p := filepath.Join(dir, "survival", "payload")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "x"), []byte(`{"ram":999999}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("panel", func(t *testing.T) {
		sm := NewStorageManager(t.TempDir(), nil)
		h := NewStreamHandler(sm)
		const uuid = "44444444-4444-4444-4444-444444444444"
		dir := sm.GetServerDir(uuid)
		cfg := plantLinks(t, dir)
		plantPayload(t, dir)
		h.handleCopy("r1", uuid, &nodepb.CopyFileReq{SrcPath: "survival/payload", DstPath: "survival"})
		assertUntouched(t, dir, cfg)
	})
	t.Run("beam", func(t *testing.T) {
		bs, uuid, ctx := newTestBeamServer(t)
		dir := bs.storageMgr.GetServerDir(uuid)
		cfg := plantLinks(t, dir)
		plantPayload(t, dir)
		bs.CopyFile(ctx, &pb.BeamFileCopyReq{SrcPath: "survival/payload", DstPath: "survival"})
		assertUntouched(t, dir, cfg)
	})
}

// A copy that does not meet a link still lands, and in the right place.
func TestCopyIntoASubServerStillCopies(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := NewStreamHandler(sm)
	const uuid = "55555555-5555-5555-5555-555555555555"
	dir := sm.GetServerDir(uuid)
	if err := os.MkdirAll(filepath.Join(dir, "survival", "plugins", "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "survival", "plugins", "cfg", "a.yml"), []byte("a"), 0o644)
	if msg := h.handleCopy("r1", uuid, &nodepb.CopyFileReq{SrcPath: "survival/plugins", DstPath: "creative"}); msg.GetError() != nil {
		t.Fatalf("copy: %v", msg.GetError())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "creative", "cfg", "a.yml")); string(b) != "a" {
		t.Fatalf("copied file = %q", b)
	}
}

// Everything the node writes on a tenant's behalf belongs to the container's
// uid. The start-time repair skips a sub-server whose directory already does,
// so a name created as root there stayed unwritable to the running server.
func TestTenantWritesAreTheContainers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	t.Setenv("MC_RUN_AS", "1000")
	owned := func(t *testing.T, p string) {
		t.Helper()
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !ownedBy(fi, 1000) {
			t.Errorf("%s is root's", p)
		}
	}

	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	sub := filepath.Join(dir, "survival")
	os.MkdirAll(sub, 0o755)
	os.Lchown(sub, 1000, 1000)

	bs.CreateFile(ctx, &pb.BeamFileCreateReq{Path: "survival/newdir", IsDir: true})
	bs.CreateFile(ctx, &pb.BeamFileCreateReq{Path: "survival/new.txt"})
	bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/saved.yml", Content: "a: 1"})
	owned(t, filepath.Join(sub, "newdir"))
	owned(t, filepath.Join(sub, "new.txt"))
	owned(t, filepath.Join(sub, "saved.yml"))

	if err := writeEULA(dir, "survival"); err != nil {
		t.Fatal(err)
	}
	owned(t, filepath.Join(sub, "eula.txt"))

	sm := bs.storageMgr
	vfs := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, sm, nil, "u")
	if err := vfs.Filecmd(&sftp.Request{Method: "Mkdir", Filepath: "s/survival/sftpdir"}); err != nil {
		t.Fatal(err)
	}
	owned(t, filepath.Join(sub, "sftpdir"))
}

// A hard link to a node file must not be handed over through the tenant's
// name for it. Made here as root; fs.protected_hardlinks stops uid 1000 from
// making one on most hosts, not on every BYON host.
func TestOwnershipWalkSkipsAHardLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "survival")
	os.MkdirAll(sub, 0o755)
	cfg := filepath.Join(root, ".node_config.json")
	os.WriteFile(cfg, []byte("{}"), 0o644)
	if err := os.Link(cfg, filepath.Join(sub, "x")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sub, "plain"), []byte("p"), 0o644)
	if err := chownSubServerTree(sub, 1000); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(cfg); ownedBy(fi, 1000) {
		t.Fatal(".node_config.json was handed to the tenant through a hard link")
	}
	if fi, _ := os.Lstat(filepath.Join(sub, "plain")); !ownedBy(fi, 1000) {
		t.Fatal("an ordinary file was not handed over")
	}
}

// Installers that run a JVM do so as the container's uid, in a directory the
// node created as root. Without a Docker daemon the call site is asserted.
func TestInstallerContainerGetsItsDirectory(t *testing.T) {
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := cutFunc(string(b), "func (dm *DockerManager) RunInstallerContainer(ctx context.Context, serverUUID, subServerName, image string, cmd []string) (string, error) {")
	if !ok {
		t.Fatal("RunInstallerContainer is gone; move this assertion with it")
	}
	hand := strings.Index(body, "handInstalledTree(")
	start := strings.Index(body, "ContainerStart(")
	if hand < 0 || start < 0 || hand > start {
		t.Error("the installer container starts before its directory is handed to the container's uid")
	}
}

// A mod's jar takes its final name by rename; it is handed over first.
func TestModInstallHandsTheJarOver(t *testing.T) {
	b, err := os.ReadFile("install_mod.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	chown := strings.Index(s, "os.Lchown(tmpFile")
	rename := strings.Index(s, "os.Rename(tmpFile, destFile)")
	if chown < 0 || rename < 0 || chown > rename {
		t.Error("a mod jar replaces the server's own without being handed over first")
	}
}

// The per-write hand-over has the same hole as the walk: a save through a
// tenant's hard link to a node file would hand the node file over.
func TestHandOverSkipsAHardLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	t.Setenv("MC_RUN_AS", "1000")
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".node_config.json")
	os.WriteFile(cfg, []byte("{}"), 0o644)
	sub := filepath.Join(dir, "survival")
	os.MkdirAll(sub, 0o755)
	if err := os.Link(cfg, filepath.Join(sub, "x")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(sub)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	chownForMCIn(root, "x")
	if fi, _ := os.Lstat(cfg); ownedBy(fi, 1000) {
		t.Fatal(".node_config.json was handed to the tenant through a hard link")
	}
}
