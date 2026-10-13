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

// A setup or reinstall that finishes after the owner was cut off used to boot
// the server anyway; both now ask the intent before they start it, and report
// "starting" when they did start it.
func TestInstallsStartOnlyWhileThePlatformWantsTheServerUp(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	setup := between(t, s, "\tcase \"setup\":", "\tcase \"reconfigure\":")
	if !strings.Contains(setup, "desired == \"stopped\" {\n\t\t\tstartIt = false") || !strings.Contains(setup, "finalStatus = \"starting\"") {
		t.Error("setup starts a server the platform no longer means to run, or reports it stopped after starting it")
	}
	reinstall := after(t, s, "\tcase \"reinstall\":")
	if !strings.Contains(reinstall, "reinstallRecreate = dm.RecreateStopped") {
		t.Error("reinstall starts a server the platform no longer means to run")
	}
}
