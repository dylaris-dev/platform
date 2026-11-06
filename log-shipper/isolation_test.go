package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server process used to inherit the shipper's Redis login, which let a
// plugin fill the shared Redis for every tenant.
func TestTheServerProcessGetsNoRedisCredential(t *testing.T) {
	env := childEnv([]string{
		"REDIS_ADDR=10.0.0.10:6379", "REDIS_USER=shipper-x", "REDIS_PASS=secret",
		"REDIS_PASS_FILE=/run/s", "REDIS_DB=0", netGateEnv + "=/data/.x",
		"SERVER_UUID=abc", "TZ=Europe/Berlin", "PATH=/usr/bin",
	})
	joined := strings.Join(env, "\n")
	for _, gone := range []string{"REDIS_", "secret", netGateEnv} {
		if strings.Contains(joined, gone) {
			t.Errorf("child env still carries %q:\n%s", gone, joined)
		}
	}
	for _, kept := range []string{"SERVER_UUID=abc", "TZ=Europe/Berlin", "PATH=/usr/bin", "TERM=xterm-256color"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("child env lost %q:\n%s", kept, joined)
		}
	}
}

func TestTheNetGateWaitsForTheNodeAndThenGivesUp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ready")
	if waitForNetGate(p, 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("the gate opened without the file")
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		os.WriteFile(p, nil, 0o644)
	}()
	if !waitForNetGate(p, 2*time.Second, 10*time.Millisecond) {
		t.Fatal("the gate did not open once the file appeared")
	}

	// L37: only the node's regular file opens it, not a directory a root
	// tenant left at the name.
	d := filepath.Join(t.TempDir(), "ready")
	os.Mkdir(d, 0o755)
	if waitForNetGate(d, 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("a directory opened the gate")
	}
}
