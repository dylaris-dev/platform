package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Every identifier pair a branch may read is checked, not only the Config one.
func TestCommandRefusalChecksTheTopLevelPairToo(t *testing.T) {
	valid := "11111111-2222-3333-4444-555555555555"
	cases := []struct {
		name   string
		cmd    NodeCommand
		refuse bool
	}{
		{"config pair fine", NodeCommand{Config: ServerConfig{UUID: valid}}, false},
		{"top-level pair fine", NodeCommand{ServerUUID: valid, SubServer: "survival"}, false},
		{"valid config, dot top-level", NodeCommand{Config: ServerConfig{UUID: valid}, ServerUUID: "."}, true},
		{"valid config, traversing sub-server", NodeCommand{Config: ServerConfig{UUID: valid}, ServerUUID: valid, SubServer: ".."}, true},
		{"valid config, sub-server without a server", NodeCommand{Config: ServerConfig{UUID: valid}, SubServer: "survival"}, true},
	}
	for _, c := range cases {
		if got := commandRefusal(c.cmd) != ""; got != c.refuse {
			t.Errorf("%s: refused=%v, want %v", c.name, got, c.refuse)
		}
	}
}

// A whole-server restore drops the node's own files from the archive even when
// there is no live copy to carry over.
func TestRestoreDropsNodeFilesTheArchiveCarried(t *testing.T) {
	stashed, restored := t.TempDir(), t.TempDir()
	for _, name := range []string{".node_config.json", ".dylaris.json"} {
		if err := os.WriteFile(filepath.Join(restored, name), []byte(`{"memory":999999}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stashed, ".dylaris.json"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := carryArchivesAcrossSwap(stashed, restored); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(restored, ".node_config.json")); !os.IsNotExist(err) {
		t.Error("the archive's .node_config.json survived a restore with no live copy")
	}
	if b, _ := os.ReadFile(filepath.Join(restored, ".dylaris.json")); string(b) != "live" {
		t.Errorf(".dylaris.json = %q, want the live copy", b)
	}

	// No server root before the restore: the archive is the only copy left.
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, ".active_server"), []byte("survival"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := carryArchivesAcrossSwap(filepath.Join(stashed, "missing"), empty); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(empty, ".active_server")); err != nil {
		t.Error("a recovery onto an empty disk lost the archive's .active_server")
	}
}

// A host port is never a privileged one or one of the node's own listeners.
func TestHostPortProblem(t *testing.T) {
	for _, env := range []string{"SFTP_PORT", "BEAM_GRPC_PORT", "MIGRATION_PORT", "BEAM_LAN_PORT"} {
		t.Setenv(env, "")
	}
	for port, bad := range map[int]bool{
		22: true, 1023: true, 1024: false, 25520: true, 25521: true, 25522: true, 25523: true,
		25600: false, 25565: false, 65535: false, 65536: true, 0: true,
	} {
		if got := hostPortProblem(port) != ""; got != bad {
			t.Errorf("port %d: refused=%v, want %v", port, got, bad)
		}
	}
}
