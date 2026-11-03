package main

import (
	"testing"

	pb "dylaris-proto/node"
)

// The sub-server count asks for names only, so the node must not walk the
// folders for their sizes. A plain listing still reports them.
func TestListNamesOnlySkipsFolderSizes(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	sizeOf := func(req *pb.ListFilesReq) int64 {
		t.Helper()
		for _, f := range h.handleList("ls", uuid, req).GetListResp().GetFiles() {
			if f.Name == "survival" {
				return f.Size
			}
		}
		t.Fatal("survival missing from the listing")
		return 0
	}
	full := sizeOf(&pb.ListFilesReq{})
	if full < int64(len("motd=hi")+len("JAR")) {
		t.Fatalf("plain listing reported %d bytes for survival, want the recursive size", full)
	}
	if got := sizeOf(&pb.ListFilesReq{NamesOnly: true}); got == full {
		t.Fatalf("names-only listing still computed the recursive size (%d)", got)
	}
}
