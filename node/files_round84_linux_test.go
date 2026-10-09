//go:build linux

package main

import (
	"dylaris-pkg/fileperms"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	beampb "dylaris-proto/beam"
	pb "dylaris-proto/node"
	"github.com/pkg/sftp"
)

// A named pipe in mods/ - a plugin can mkfifo - blocked the hash open forever,
// on the read loop of the node's Core connection.
func TestHashingAPipeDoesNotBlock(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	if err := syscall.Mkfifo(filepath.Join(root, "survival", "plugins", "a.jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan *pb.NodeMessage, 1)
	go func() {
		done <- h.handleHashFiles("h", uuid, &pb.HashFilesReq{Path: "survival/plugins", Names: []string{"a.jar", "ok.jar"}})
	}()
	select {
	case resp := <-done:
		files := resp.GetHashFilesResp().GetFiles()
		if len(files) != 2 || files[0].Error == "" || files[1].Sha1 == "" {
			t.Fatalf("got %+v", files)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hashing a pipe blocked")
	}
}

// The same files through a link a plugin plants inside its own folder: every
// read path followed it to the node's config and the backup store.
func TestTheNodesOwnFilesAreNotReadableThroughALink(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	for link, target := range map[string]string{
		"survival/cfg.json": "../.node_config.json",
		"survival/bk.tgz":   "../.dylaris-backups/job-1/a.tar.gz",
		"survival/bkdir":    "../.dylaris-backups/job-1",
	} {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}

	for _, p := range []string{"survival/cfg.json", "survival/bk.tgz"} {
		body, code := readVia(h, uuid, &pb.NodeMessage{RequestId: "r", Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: p}}})
		if code == 0 || len(body) != 0 {
			t.Errorf("read %s: code %d, body %q", p, code, body)
		}
	}
	body, code := readVia(h, uuid, &pb.NodeMessage{RequestId: "z", Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: "survival", ZipIfDir: true}}})
	if code != 0 {
		t.Fatalf("zip: error %d", code)
	}
	if names := strings.Join(zipNames(t, body), " "); strings.Contains(names, "cfg.json") || strings.Contains(names, "bk.tgz") || !strings.Contains(names, "ok.jar") {
		t.Errorf("the archive followed a link onto a node file: %s", names)
	}
	if files := h.handleList("l", uuid, &pb.ListFilesReq{Path: "survival/bkdir"}).GetListResp().GetFiles(); len(files) != 0 {
		t.Errorf("the backup store was listed through a link: %v", files)
	}
	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/cfg.json", DstPath: "survival/copy.json"}); resp.GetError() == nil {
		t.Error("a copy materialised the node's config as a tenant file")
	}
	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival", DstPath: "creative"}); resp.GetError() != nil {
		t.Fatalf("dir copy: %v", resp.GetError())
	}
	if _, err := os.Lstat(filepath.Join(root, "creative", "cfg.json")); err == nil {
		t.Error("a directory copy carried the node's config along")
	}
	resp := h.handleHashFiles("h", uuid, &pb.HashFilesReq{Path: "survival", Names: []string{"cfg.json"}})
	if f := resp.GetHashFilesResp().GetFiles(); len(f) != 1 || f[0].Sha1 != "" {
		t.Errorf("hashed the node's config: %+v", f)
	}
}

// Beam and SFTP read the same server directory: the node's config and the
// backup store through a link are refused there too.
func TestBeamAndSFTPDoNotReadTheNodesConfigThroughALink(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	for name, body := range map[string]string{
		".node_config.json":               `{"extraJvmFlags":"-Dsecret=1"}`,
		".dylaris-backups/job-1/a.tar.gz": "ARCHIVE",
		"survival/ok.txt":                 "ok",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../.node_config.json", filepath.Join(dir, "survival", "cfg.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.dylaris-backups/job-1", filepath.Join(dir, "survival", "bkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.dylaris-backups/job-1/a.tar.gz", filepath.Join(dir, "survival", "bk.tgz")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"survival/cfg.json", ".node_config.json"} {
		resp, err := bs.ReadFileContent(ctx, &beampb.BeamFileReadReq{Path: p})
		if err == nil && resp.Success {
			t.Errorf("beam read %s: %q", p, resp.Content)
		}
	}
	for _, p := range []string{".dylaris-backups/job-1/a.tar.gz", "survival/bk.tgz"} {
		if resp, err := bs.ReadFileContent(ctx, &beampb.BeamFileReadReq{Path: p}); err == nil && resp.Success {
			t.Errorf("beam read the backup %s", p)
		}
	}
	if resp, _ := bs.ListFiles(ctx, &beampb.BeamFileListReq{Path: "survival/bkdir"}); len(resp.GetFiles()) != 0 {
		t.Errorf("beam listed the backup store through a link: %v", resp.GetFiles())
	}
	st := &fakeBeamDownloadStream{ctx: ctx}
	if err := bs.DownloadFile(&beampb.BeamDownloadReq{Path: "survival/bk.tgz"}, st); err == nil || st.buf.Len() != 0 {
		t.Errorf("beam downloaded the backup through a link: %v", err)
	}
	st = &fakeBeamDownloadStream{ctx: ctx}
	if err := bs.DownloadFile(&beampb.BeamDownloadReq{Path: "survival", ZipIfDir: true}, st); err != nil {
		t.Fatalf("zip: %v", err)
	}
	if names, _ := zipEntries(t, st.buf.Bytes()); strings.Contains(strings.Join(names, " "), "bk") {
		t.Errorf("beam zipped the backup store through a link: %v", names)
	}
	if resp, err := bs.CopyFile(ctx, &beampb.BeamFileCopyReq{SrcPath: "survival/bk.tgz", DstPath: "survival/copy.tgz"}); err == nil && resp.Success {
		t.Error("beam copied the backup out through a link")
	}
	if resp, err := bs.ReadFileContent(ctx, &beampb.BeamFileReadReq{Path: "survival/ok.txt"}); err != nil || !resp.Success {
		t.Errorf("beam no longer reads an ordinary file: %v %+v", err, resp)
	}

	fsys := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, bs.storageMgr, nil, "tester")
	if _, err := fsys.Fileread(&sftp.Request{Method: "Get", Filepath: "s/survival/cfg.json"}); err == nil {
		t.Error("sftp read the node's config through a link")
	}
	if _, err := fsys.Fileread(&sftp.Request{Method: "Get", Filepath: "s/survival/ok.txt"}); err != nil {
		t.Errorf("sftp no longer reads an ordinary file: %v", err)
	}
}

// The container mounts the whole server directory, so the node's own files were
// readable to any plugin straight off the mount. They are root-only now, and a
// container start tightens the ones written before.
func TestTheNodesOwnFilesAreRootOnly(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{".node_config.json", ".dylaris.json", ".active_server"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, backupDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	restrictNodeOwnedFiles(dir)
	for n, want := range map[string]os.FileMode{".node_config.json": 0o600, ".dylaris.json": 0o600, ".active_server": 0o600, backupDirName: 0o700} {
		info, err := os.Stat(filepath.Join(dir, n))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", n, info.Mode().Perm(), err, want)
		}
	}

	saveNodeConfig(dir, ServerConfig{UUID: "u"})
	os.Remove(filepath.Join(dir, ".node_config.json"))
	saveNodeConfig(dir, ServerConfig{UUID: "u"})
	if info, err := os.Stat(filepath.Join(dir, ".node_config.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("a new config is written %v", info.Mode().Perm())
	}
}

// SFTP listed the backup store through a link onto it.
func TestSFTPDoesNotListTheBackupStoreThroughALink(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	if err := os.Symlink("../.dylaris-backups/job-1", filepath.Join(root, "survival", "bkdir")); err != nil {
		t.Fatal(err)
	}
	fsys := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, h.storageMgr, nil, "tester")
	if l, err := fsys.Filelist(&sftp.Request{Method: "List", Filepath: "s/survival/bkdir"}); err == nil {
		buf := make([]os.FileInfo, 8)
		if n, _ := l.ListAt(buf, 0); n != 0 {
			t.Errorf("listed %d entries of the backup store", n)
		}
	}
}

// Listing judges the directory it opened, opened without blocking: a named
// pipe a plugin leaves where a folder was expected fails instead of hanging.
func TestListingAPipeDoesNotBlock(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	if err := syscall.Mkfifo(filepath.Join(root, "survival", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		h.handleList("l", uuid, &pb.ListFilesReq{Path: "survival/pipe"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listing a pipe blocked")
	}
}
