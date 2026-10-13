package main

import (
	"os"
	"strings"
	"testing"
)

// The wizard's storage path was dropped: create always let the node choose. It
// now asks for the chosen path, and falls back to the node's own choice when
// the path is gone, rather than leaving a server row with no directory.
func TestCreateUsesTheChosenStoragePath(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	body := between(t, s, "\tcase \"create\":", "\tcase \"setup\":")
	if !strings.Contains(body, "storage.SelectStoragePath(cmd.Config.UUID, cmd.Config.StoragePath)") ||
		!strings.Contains(body, "storagePath, err = storage.SelectStoragePath(cmd.Config.UUID, \"\")") {
		t.Error("create ignores the chosen storage path or has no fallback")
	}
}
