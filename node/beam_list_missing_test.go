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
