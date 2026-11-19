package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/pkg/sftp"
	"google.golang.org/grpc/peer"
)

// permittedFS builds a virtualFS over one real server directory for an account
// holding exactly `p`.
func permittedFS(t *testing.T, p fileperms.Perms) (*virtualFS, string) {
	t.Helper()
	sm, _ := newPlacementManager(t, 1)
	// A delete remeasures in the background, reading globalStorageMgr, which
	// later tests swap.
	t.Cleanup(func() { waitRemeasuresIdle(t) })
	const uuid = "srv-uuid"
	base := filepath.Join(sm.Paths()[0], uuid, "survival")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "server.jar")
	if err := os.WriteFile(target, []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "myserver", Perms: p}}, sm, nil, "tester")
	return fs, target
}

// The Builder role is write-but-not-delete, and this is the door that ignored
// the difference.
//
// SFTP asked one question - is this account allowed a session - and then
// permitted every operation through it, so an account invited as a Builder was
// refused a delete over HTTP and could remove server.jar here. The permissions
// now come out of resolve() alongside the path, published per (node, user) by
// Core from the same resolution the HTTP file API enforces.
func TestSFTPEnforcesTheFileVerbs(t *testing.T) {
	builder := fileperms.Perms{Read: true, Write: true}

	t.Run("a builder may not delete", func(t *testing.T) {
		fs, target := permittedFS(t, builder)
		err := fs.Filecmd(&sftp.Request{Method: "Remove", Filepath: "myserver/survival/server.jar"})
		if err == nil {
			t.Fatal("a Builder deleted server.jar over SFTP; the same account is refused this over HTTP")
		}
		if _, serr := os.Stat(target); serr != nil {
			t.Errorf("the file is gone even though the call reported an error: %v", serr)
		}
	})

	t.Run("a builder may still write", func(t *testing.T) {
		fs, _ := permittedFS(t, builder)
		w, err := fs.Filewrite(&sftp.Request{Method: "Put", Filepath: "myserver/survival/new.txt"})
		if err != nil {
			t.Fatalf("a Builder was refused a write, which their role does grant: %v", err)
		}
		if c, ok := w.(interface{ Close() error }); ok {
			c.Close()
		}
	})

	t.Run("a reader may not write and may not create directories", func(t *testing.T) {
		fs, _ := permittedFS(t, fileperms.Perms{Read: true})
		if _, err := fs.Filewrite(&sftp.Request{Method: "Put", Filepath: "myserver/survival/new.txt"}); err == nil {
			t.Error("a read-only account wrote a file")
		}
		if err := fs.Filecmd(&sftp.Request{Method: "Mkdir", Filepath: "myserver/survival/sub"}); err == nil {
			t.Error("a read-only account created a directory")
		}
	})

	t.Run("an account with delete may delete", func(t *testing.T) {
		fs, target := permittedFS(t, fileperms.Full())
		if err := fs.Filecmd(&sftp.Request{Method: "Remove", Filepath: "myserver/survival/server.jar"}); err != nil {
			t.Fatalf("an account holding files.delete was refused: %v", err)
		}
		if _, serr := os.Stat(target); serr == nil {
			t.Error("the delete reported success and the file is still there")
		}
	})

	// An entry published by a Core that predates the permissions decodes with
	// all three false. Refusing is the safe direction and the next 60s sync
	// replaces the entry.
	t.Run("an entry with no permissions grants nothing", func(t *testing.T) {
		fs, _ := permittedFS(t, fileperms.Perms{})
		if _, err := fs.Fileread(&sftp.Request{Method: "Get", Filepath: "myserver/survival/server.jar"}); err == nil {
			t.Error("an entry carrying no permissions allowed a read")
		}
	})
}

// The same rule on the other surface. Beam has no username to look anything up
// with - it sees a signed ticket and nothing else - so the permissions travel in
// the ticket, and the node binds them per connection exactly as it already binds
// the server UUID and the username.
func TestBeamEnforcesTheFileVerbs(t *testing.T) {
	newSession := func(t *testing.T, port int, perms *fileperms.Perms) (*beamServer, context.Context, string) {
		t.Helper()
		sm := NewStorageManager(t.TempDir(), nil)
		bs := &beamServer{storageMgr: sm, throttle: NewBeamThrottle(context.Background(), nil)}
		const uuid = "11111111-1111-1111-1111-111111111111"
		addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
		bs.serverUUIDByPeer.Store(addr.String(), uuid)
		bindTestSession(bs, addr.String())
		bs.permsByPeer.Store(addr.String(), perms)
		dir := filepath.Join(sm.GetServerDir(uuid), "survival")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "server.jar"), []byte("jar"), 0o644); err != nil {
			t.Fatal(err)
		}
		return bs, ctx, filepath.Join(dir, "server.jar")
	}

	t.Run("a builder may not delete", func(t *testing.T) {
		builder := fileperms.Perms{Read: true, Write: true}
		bs, ctx, target := newSession(t, 42001, &builder)
		if _, err := bs.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: "survival/server.jar"}); err == nil {
			t.Fatal("a Builder deleted server.jar through the beam client; DeleteFile calls os.RemoveAll")
		}
		if _, serr := os.Stat(target); serr != nil {
			t.Errorf("the file is gone even though the call was refused: %v", serr)
		}
	})

	t.Run("a reader may not write", func(t *testing.T) {
		reader := fileperms.Perms{Read: true}
		bs, ctx, _ := newSession(t, 42002, &reader)
		if _, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/x.txt", Content: "x"}); err == nil {
			t.Error("a read-only account saved a file")
		}
	})

	// A ticket minted by a Core older than this field carries no permissions at
	// all. That is refused, and it is worth telling apart from a ticket granting
	// none: the fix is to update Core, not to change anyone's role.
	t.Run("a ticket with no permissions is refused, and says why", func(t *testing.T) {
		bs, ctx, _ := newSession(t, 42003, nil)
		_, err := bs.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: "survival/server.jar"})
		if err == nil {
			t.Fatal("a ticket carrying no permissions was allowed to delete")
		}
		if got := err.Error(); !strings.Contains(got, "older than this node") {
			t.Errorf("the refusal does not name the cause an operator can act on: %s", got)
		}
	})

	t.Run("an unauthenticated peer is refused", func(t *testing.T) {
		sm := NewStorageManager(t.TempDir(), nil)
		bs := &beamServer{storageMgr: sm}
		addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 42004}
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
		if _, err := bs.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: "survival/server.jar"}); err == nil {
			t.Error("a peer that never authenticated was allowed to delete")
		}
	})
}
