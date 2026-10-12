package main

import (
	"os"
	"path/filepath"
	"testing"

	beampb "dylaris-proto/beam"
	nodepb "dylaris-proto/node"
)

// protectedNames is the set isProtectedFile defends. Every write path that
// takes a caller-supplied DESTINATION name has to refuse all of them, not a
// hand-picked one.
var protectedNames = []string{".active_server", ".node_config.json", ".dylaris.json", ".dylaris-backups"}

// A beam ticket scopes a session to ONE server, and every path in it is
// measured against that server's directory. RenameFile has two paths, and the
// destination is the one that leaves the directory: it is not a request path at
// all, so it never reaches validateBeamPath.
//
// The neighbouring server is on the same storage path, so os.Rename across the
// two succeeds. Moving a file the attacker owns into a neighbour's plugins/ or
// mods/ folder is code the neighbour's Minecraft server then loads.
func TestBeamRenameCannotEscapeTheServerDirectory(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)

	serverDir := bs.storageMgr.GetServerDir(serverUUID)
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(serverDir, "payload.jar")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	const victimUUID = "22222222-2222-2222-2222-222222222222"
	victimPlugins := filepath.Join(bs.storageMgr.GetServerDir(victimUUID), "plugins")
	if err := os.MkdirAll(victimPlugins, 0o755); err != nil {
		t.Fatal(err)
	}

	resp, err := bs.RenameFile(ctx, &beampb.BeamFileRenameReq{
		OldPath: "payload.jar",
		NewName: filepath.Join("..", victimUUID, "plugins", "payload.jar"),
	})
	if err != nil {
		t.Fatalf("RenameFile returned a transport error: %v", err)
	}
	if resp.Success {
		t.Errorf("a rename out of the server directory was accepted: %q", resp.Message)
	}
	if _, statErr := os.Stat(filepath.Join(victimPlugins, "payload.jar")); statErr == nil {
		t.Error("the file landed in another server's plugins directory")
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Errorf("the source file was moved away despite the rename being refused: %v", statErr)
	}
}

// The same destination check the other beam write paths make: CopyFile runs its
// destination through validateBeamPath, RenameFile did not run its own through
// anything.
func TestBeamRenameCannotOverwriteAProtectedName(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)

	serverDir := bs.storageMgr.GetServerDir(serverUUID)
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range protectedNames {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(serverDir, "innocent.txt")
			if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			resp, err := bs.RenameFile(ctx, &beampb.BeamFileRenameReq{
				OldPath: "innocent.txt",
				NewName: name,
			})
			if err != nil {
				t.Fatalf("RenameFile returned a transport error: %v", err)
			}
			if resp.Success {
				t.Errorf("rename onto the platform-managed name %q was accepted", name)
			}
			if _, statErr := os.Stat(filepath.Join(serverDir, name)); statErr == nil {
				t.Errorf("%s now exists, written by a rename", name)
			}
			os.Remove(src)
		})
	}
}

// gRPC's handleRename only accepts a bare filename, so it cannot traverse -
// but its protected-name check was one hand-written string compare while
// isProtectedFile knows four names plus two prefixes, and every one of them is
// a bare filename.
func TestGRPCRenameCannotOverwriteAProtectedName(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := NewStreamHandler(sm)
	const serverUUID = "33333333-3333-3333-3333-333333333333"

	serverDir := sm.GetServerDir(serverUUID)
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range protectedNames {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(serverDir, "innocent.txt")
			if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			msg := h.handleRename("req-1", serverUUID, &nodepb.RenameFileReq{
				OldPath: "innocent.txt",
				NewName: name,
			})
			if msg.GetError() == nil {
				t.Errorf("rename onto the platform-managed name %q was accepted", name)
			}
			if _, statErr := os.Stat(filepath.Join(serverDir, name)); statErr == nil {
				t.Errorf("%s now exists, written by a rename", name)
			}
			os.Remove(src)
		})
	}
}
