package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R35-02/R35-04: every path that recreates the container on a sub-server
// records that sub-server in .active_server before the recreate, as setup and
// switch do. A reinstall or version move into a sub-server the node had not
// switched to left the old name there, and later switch reports and deletes
// read it. Source check: the branches need a real Docker to succeed.
func TestAnInstallThatRecreatesTheContainerRecordsTheActiveSubServer(t *testing.T) {
	const write = `writeServerRootFile(serverPath, ".active_server"`
	for _, c := range []struct{ file, from, to, recreate string }{
		{"main.go", `case "reinstall":`, `case "migrate_storage":`, "reinstallRecreate(cmd.Config)"},
		{"update_server_version.go", "stash.discard()", "func abandonVersionUpdate", "recreate(cmd.Config)"},
	} {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		body := between(t, string(b), c.from, c.to)
		w, r := strings.Index(body, write), strings.Index(body, c.recreate)
		if w < 0 || r < 0 || w > r {
			t.Errorf("%s %s: .active_server is not written before the container is recreated", c.file, c.from)
		}
	}
}

// The heartbeat names each held server's .active_server so Core can follow the
// node; a server without a readable plain name is left out (unknown).
func TestActiveSubServersReportsTheDiskMarker(t *testing.T) {
	root := t.TempDir()
	const (
		withName = "35353535-0000-0000-0000-000000000001"
		noFile   = "35353535-0000-0000-0000-000000000002"
		notAName = "35353535-0000-0000-0000-000000000003"
	)
	for uuid, content := range map[string]string{withName: "main\n", noFile: "", notAName: "../x"} {
		dir := filepath.Join(root, uuid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	sm := &StorageManager{paths: []string{root}}
	infos := []StorageInfo{
		{Path: root, ServerUUIDs: []string{withName, noFile, notAName}},
		{Path: "/gone", ServerUUIDs: []string{"35353535-0000-0000-0000-000000000004"}, unreadable: true},
	}
	got := activeSubServersOf(sm, infos)
	if len(got) != 1 || got[withName] != "main" {
		t.Errorf("activeSubServersOf = %v, want only %s=main", got, withName)
	}
	if activeSubServersOf(nil, infos) != nil {
		t.Error("no storage manager must report nothing")
	}
}
