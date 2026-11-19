package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// R29-02: a sub-server delete took no power lock, so a start or restart
// running beside it rebuilt the container on the slot it had just renamed
// away, and Docker made the directory again, root-owned. The delete now waits
// for the power action to finish.
func TestASubServerDeleteWaitsForThePowerAction(t *testing.T) {
	const uuid = "29292929-0202-0202-0202-292929292929"
	root := t.TempDir()
	sub := filepath.Join(root, uuid, "survival")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	release, ok := tryLockServerPower(uuid)
	if !ok {
		t.Fatal("power lock taken")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd := NodeCommand{Action: "delete_sub_server"}
		cmd.Config.UUID = uuid
		cmd.Config.ActiveSubServer = "survival"
		processCommand(context.Background(), cmd, "", newMiniRedis(t), dockerWithWorkingDir(t, ""), "node-a", nil, &StorageManager{paths: []string{root}})
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("the delete went ahead while a power action held the server: %v", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the delete did not run once the power action ended")
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Errorf("sub-server still on disk after the delete: %v", err)
	}
}
