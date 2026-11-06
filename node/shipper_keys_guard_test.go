package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// M04: the directory scan is the tenant's /data, so plugin code can create as
// many directories as it has inodes. Each one used to cost a Redis round trip
// every pass, for every server on the node, in one goroutine.
func TestShipperGuardSubServersIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := range 500 {
		os.Mkdir(filepath.Join(dir, fmt.Sprintf("a%04d", i)), 0o755)
	}
	os.Mkdir(filepath.Join(dir, "bad name"), 0o755)
	os.Mkdir(filepath.Join(dir, ".hidden"), 0o755)
	os.Mkdir(filepath.Join(dir, "zz-active"), 0o755)
	os.WriteFile(filepath.Join(dir, ".active_server"), []byte("zz-active\n"), 0o644)

	subs := shipperGuardSubServers(dir)
	if len(subs) > shipperGuardMaxSubServers {
		t.Fatalf("%d sub-server keys per pass, want at most %d", len(subs), shipperGuardMaxSubServers)
	}
	if len(subs) == 0 || subs[0] != "zz-active" {
		t.Fatalf("the active sub-server (sorted last) must always be guarded, got %v", subs[:min(3, len(subs))])
	}
	for _, s := range subs {
		if s == "bad name" || s == ".hidden" {
			t.Fatalf("%q is not a sub-server name and was guarded", s)
		}
	}
}
