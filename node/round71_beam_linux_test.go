package main

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"

	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
	"golang.org/x/sys/unix"
)

// plantTopLink adds what the tenant's server can make in its own directory: a
// link whose target names the node's config once it sits at the server root,
// and a file of that name beside it so the link resolves where it is.
func plantTopLink(t *testing.T, serverDir string) string {
	t.Helper()
	cfg := plantLinks(t, serverDir)
	if err := os.WriteFile(filepath.Join(serverDir, "survival", ".node_config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".node_config.json", filepath.Join(serverDir, "survival", "z")); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A rename moved the link itself to the top of the server, where its target
// is the node's config, and the next save wrote through it.
func TestBeamRenameDoesNotMoveALinkToTheTop(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	cfg := plantTopLink(t, dir)

	r, _ := bs.RenameFile(ctx, &pb.BeamFileRenameReq{OldPath: "survival/z", NewName: "../y"})
	if r.GetSuccess() {
		t.Error("a link was renamed to the server root")
	}
	bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "y", Content: `{"ram":999999}`})
	assertUntouched(t, dir, cfg)
	if _, err := os.Lstat(filepath.Join(dir, "survival", "z")); err != nil {
		t.Errorf("the refused rename lost the link: %v", err)
	}
}

func TestSFTPRenameDoesNotMoveALinkToTheTop(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	const uuid = "44444444-4444-4444-4444-444444444444"
	dir := sm.GetServerDir(uuid)
	cfg := plantTopLink(t, dir)
	fs := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, sm, nil, "u")

	if err := fs.Filecmd(&sftp.Request{Method: "Rename", Filepath: "s/survival/z", Target: "s/y"}); err == nil {
		t.Error("a link was renamed to the server root")
	}
	if w, err := fs.Filewrite(&sftp.Request{Method: "Put", Filepath: "s/y", Flags: 0x02 | 0x08 | 0x10}); err == nil {
		w.WriteAt([]byte("{}"), 0)
		w.(interface{ Close() error }).Close()
	}
	assertUntouched(t, dir, cfg)
}

// The second half, for a link that got there anyway (a rename that lost the
// race to a swap): a write at the server root does not follow it.
func TestWriteAtTheServerRootDoesNotFollowALink(t *testing.T) {
	dir := t.TempDir()
	cfg := plantLinks(t, dir)
	if err := os.Symlink(".node_config.json", filepath.Join(dir, "y")); err != nil {
		t.Fatal(err)
	}
	if root, _, err := writeScope(dir, "y", false); err == nil {
		root.Close()
		t.Fatal("a write scope was opened on a link at the server root")
	}
	assertUntouched(t, dir, cfg)
	// A plain file there is still writable.
	root, leaf, err := writeScope(dir, "notes.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile(leaf, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A pipe the tenant's server made held every read that opened it - beam, the
// panel's download and SFTP - forever.
func TestReadsRefuseAPipe(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	os.MkdirAll(filepath.Join(dir, "survival"), 0o755)
	if err := unix.Mkfifo(filepath.Join(dir, "survival", "p"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	fs := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, bs.storageMgr, nil, "u")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for name, read := range map[string]func() bool{
		"beam": func() bool {
			r, _ := bs.ReadFileContent(ctx, &pb.BeamFileReadReq{Path: "survival/p"})
			return r.GetSuccess()
		},
		"panel": func() bool {
			ok := true
			NewStreamHandler(bs.storageMgr).streamFile("r", root, "survival/p", nil, func(m *nodepb.NodeMessage) error {
				if m.GetError() != nil {
					ok = false
				}
				return nil
			})
			return ok
		},
		"sftp": func() bool {
			_, err := fs.Fileread(&sftp.Request{Method: "Get", Filepath: "s/survival/p"})
			return err == nil
		},
	} {
		done := make(chan bool, 1)
		go func() { done <- read() }()
		select {
		case ok := <-done:
			if ok {
				t.Errorf("%s: a pipe was read", name)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: reading a pipe blocked", name)
		}
	}
}

// The tenant swaps survival/z between a file and a link while a rename to the
// top and a save there run in parallel. Checking the link after the rename
// left it under its final name long enough for the save: measured overwritten
// in 7 to 41 ms. Staged under a refused name, it is judged where nothing can
// write to it.
func TestRenameRaceCannotPlaceALinkAtTheTop(t *testing.T) {
	r := t.TempDir()
	cfg := filepath.Join(r, ".node_config.json")
	if err := os.WriteFile(cfg, []byte("ORIG"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(r, "survival")
	os.Mkdir(sub, 0o755)
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			tf, tl := filepath.Join(sub, ".tf"), filepath.Join(sub, ".tl")
			os.WriteFile(tf, []byte("x"), 0o644)
			os.Rename(tf, filepath.Join(sub, "z"))
			os.Remove(tl)
			os.Symlink(".node_config.json", tl)
			os.Rename(tl, filepath.Join(sub, "z"))
		}
	}()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			renameNoFollow(r, "survival/z", "z")
		}
	}()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if root, leaf, err := writeScope(r, "z", false); err == nil {
				root.WriteFile(leaf, []byte("PWN"), 0o644)
				root.Close()
			}
		}
	}()
	defer func() { stop.Store(true); wg.Wait() }()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if b, _ := os.ReadFile(cfg); string(b) != "ORIG" {
			t.Fatalf(".node_config.json was overwritten through a moved link: %q", b)
		}
	}
}
