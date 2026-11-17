package main

import (
	"os"
	"strings"
	"testing"
)

// setup saved .node_config.json only when its recreate succeeded. The recreate
// removes the old container first, so a failure left no container and the
// create-time config (no sub-server, no command): the reconciler marked the
// installed server pending_setup and a Start could not inspect a container.
// Pinned in the source, like reinstall's: setup needs Redis and Docker to run.
func TestSetupSavesTheConfigWhateverTheRecreateDoes(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	body := between(t, src, "if err := recreate(cmd.Config); err != nil {", "refreshServerMetadata(serverPath, cmd.Config.UUID, \"\", cmd.Config.Docker.Image, cmd.Config.Docker.RAM, cmd.Config.Docker.CPULimit, subName)")
	branches, rest, ok := strings.Cut(body, "\n\t\t}\n")
	if !ok {
		t.Fatal("cannot find the end of setup's recreate branches")
	}
	if strings.Contains(branches, "saveNodeConfig(") || !strings.Contains(rest, "saveNodeConfig(serverPath, cmd.Config)") {
		t.Error("setup saves the config only on one outcome of its recreate")
	}
}
