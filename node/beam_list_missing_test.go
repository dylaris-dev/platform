package main

import (
	"os"
	"path/filepath"
	"testing"

	pb "dylaris-proto/beam"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Beam answers a missing folder the way the panel's listing does. The folder
// list stayed empty instead, so Beam showed a folder that is not there.
func TestBeamListingAMissingFolder(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := filepath.Join(bs.storageMgr.GetServerDir(uuid), "survival")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.ListFiles(ctx, &pb.BeamFileListReq{Path: "survival/nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing folder: err %v, want NotFound", err)
	}
	if _, err := bs.ListFiles(ctx, &pb.BeamFileListReq{Path: "survival"}); err != nil {
		t.Fatalf("existing folder: %v", err)
	}
}

// Beam's listing hides exactly what isProtectedFile hides. It kept its own
// hand-written list, which missed the Technic staging directories: Beam listed
// them and then answered "not found" when one was opened.
func TestBeamListingHidesProtectedEntries(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	root := bs.storageMgr.GetServerDir(uuid)
	for _, name := range []string{".technic-stage-1", ".technic-dl-1", ".pending-delete-1", renameStagePrefix + "1", ".dylaris-backups", "world"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".dylaris.json", ".active_server", ".node_config.json", netGateFile, "server.properties"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := bs.ListFiles(ctx, &pb.BeamFileListReq{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range resp.GetFiles() {
		if isProtectedFile(f.GetName()) {
			t.Errorf("listed protected entry %q", f.GetName())
		}
	}
	if n := len(resp.GetFiles()); n != 2 {
		t.Errorf("listed %d entries, want world and server.properties", n)
	}
}
