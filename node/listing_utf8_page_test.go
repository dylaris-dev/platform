package main

import (
	"fmt"
	"testing"

	pb "dylaris-proto/node"
)

// Pages were cut by the raw names, and boundForCore repairs invalid UTF-8
// before its size check: an isolated invalid byte grows to three, so a page of
// Latin-1 names from an old zip grew past the limit and the folder became a 502.
func TestListingPagesAreCutByTheRepairedNames(t *testing.T) {
	cases := []struct {
		name    string
		entries int
		pattern string
	}{
		{"two isolated invalid bytes per name, paged", 6000, "B\xe4ume_Gr\xfcn_%05d.png"},
		{"three per name, paged", 6000, "M\xfcller_Pr\xe4s\xe9_%05d.txt"},
		{"just under the unpaged limit raw", 6100, "\xe4a\xe4b\xe4c_%05d.p"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := make([]*pb.FileInfo, c.entries)
			for i := range files {
				files[i] = &pb.FileInfo{Name: fmt.Sprintf(c.pattern, i), Size: 1}
			}
			reply := &pb.NodeMessage{RequestId: "0123456789abcdef0123456789abcdef0123", Payload: &pb.NodeMessage_ListResp{ListResp: &pb.ListFilesResp{Files: files}}}
			got := 0
			for i, part := range pageListing(reply) {
				out := boundForCore(part)
				if e := out.GetError(); e != nil {
					t.Fatalf("part %d became an error: %s", i, e.GetMessage())
				}
				got += len(out.GetListResp().GetFiles())
			}
			if got != c.entries {
				t.Fatalf("listed %d entries, want %d", got, c.entries)
			}
		})
	}
}
