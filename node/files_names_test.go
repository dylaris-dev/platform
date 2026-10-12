package main

import (
	"os"
	"path/filepath"
	"testing"

	pb "dylaris-proto/node"
)

// Minecraft names a world "New World" and mod jars carry '+'. The rename used
// to strip every character outside [A-Za-z0-9._+-], so "New World" became
// "NewWorld" with nothing said. A name is now checked, not rewritten.
func TestRenameKeepsTheNameItWasGiven(t *testing.T) {
	for _, name := range []string{"New World", "fabric-api-0.92.0+1.20.1.jar", "Übersetzung.txt", "a&b (copy).txt"} {
		t.Run(name, func(t *testing.T) {
			h, uuid, root := seedNodeOwned(t)
			msg := h.handleRename("r", uuid, &pb.RenameFileReq{OldPath: "survival/server.properties", NewName: name})
			if e := msg.GetError(); e != nil {
				t.Fatalf("refused: %d %s", e.Code, e.Message)
			}
			if _, err := os.Stat(filepath.Join(root, "survival", name)); err != nil {
				t.Fatalf("renamed file not found under its name: %v", err)
			}
		})
	}
}

func TestRenameRefusesANameThatIsAPath(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../escape", "sub/x", `win\x`, "nul\x00byte"} {
		t.Run(name, func(t *testing.T) {
			h, uuid, root := seedNodeOwned(t)
			msg := h.handleRename("r", uuid, &pb.RenameFileReq{OldPath: "survival/server.properties", NewName: name})
			if e := msg.GetError(); e == nil || e.Code != 400 {
				t.Fatalf("got %v, want a 400", msg)
			}
			if _, err := os.Stat(filepath.Join(root, "survival", "server.properties")); err != nil {
				t.Fatalf("the source moved: %v", err)
			}
		})
	}
}

// A folder that does not exist is a 404, as a read is, not an empty folder.
// The server directory itself is the one exception: setup lists the root of a
// fresh server before anything has created it, and reads an empty list as
// "nothing installed yet".
func TestListingAMissingFolder(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)

	msg := h.handleList("l", uuid, &pb.ListFilesReq{Path: "survival/nope"})
	if e := msg.GetError(); e == nil || e.Code != 404 {
		t.Fatalf("missing folder: got %v, want a 404", msg)
	}

	msg = h.handleList("l", uuid, &pb.ListFilesReq{Path: "survival"})
	if msg.GetError() != nil || len(msg.GetListResp().GetFiles()) == 0 {
		t.Fatalf("existing folder: got %v", msg)
	}

	const fresh = "85858585-8585-8585-8585-858585858585"
	for _, p := range []string{"", "survival"} {
		msg = h.handleList("l", fresh, &pb.ListFilesReq{Path: p})
		if msg.GetError() != nil || msg.GetListResp() == nil || len(msg.GetListResp().GetFiles()) != 0 {
			t.Fatalf("server dir not created yet, path %q: got %v, want an empty list", p, msg)
		}
	}
}
