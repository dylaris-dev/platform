package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPropertiesDisableStatus(t *testing.T) {
	cases := []struct {
		name, text string
		want       bool
	}{
		{"disabled", "motd=A\nenable-status=false\n", true},
		{"case and spaces", "  enable-status =  FALSE  \r\n", true},
		{"colon separator", "enable-status:false", true},
		{"enabled", "enable-status=true", false},
		{"missing", "motd=A\nonline-mode=true", false},
		{"commented out", "#enable-status=false\n! enable-status=false", false},
		{"last one wins", "enable-status=false\nenable-status=true", false},
		{"other key with the prefix", "enable-status-x=false", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := propertiesDisableStatus(strings.NewReader(c.text)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A server with the status ping turned off never answers one; the node reads
// that from its active sub-server and does not hold it on "starting".
func TestStatusPingDisabledReadsTheActiveSubServer(t *testing.T) {
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	const uuid = "srv-ready-0001"
	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	write := func(sub, text string) {
		t.Helper()
		dir := filepath.Join(root, uuid, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main", "enable-status=false\n")
	write("lobby", "enable-status=true\n")

	if !statusPingDisabled(uuid, "/data/main") {
		t.Error("main has enable-status=false: want disabled")
	}
	if statusPingDisabled(uuid, "/data/lobby") {
		t.Error("lobby answers pings: want enabled")
	}
	for _, wd := range []string{"/data/missing", "/data/../" + uuid + "/main", "/srv", ""} {
		if statusPingDisabled(uuid, wd) {
			t.Errorf("working dir %q: want false", wd)
		}
	}
	globalStorageMgr = nil
	if statusPingDisabled(uuid, "/data/main") {
		t.Error("no storage manager: want false")
	}
}
