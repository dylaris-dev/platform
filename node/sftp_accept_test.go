package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A failing accept is paced: without a pause a lasting failure (no file
// descriptors left) spun the loop at full CPU and flooded the log.
func TestSFTPAcceptPause(t *testing.T) {
	var d time.Duration
	want := []time.Duration{5, 10, 20, 40, 80, 160, 320, 640, 1000, 1000}
	for i, w := range want {
		d = nextAcceptPause(d)
		if d != w*time.Millisecond {
			t.Fatalf("pause %d = %s, want %s", i, d, w*time.Millisecond)
		}
	}
}

// Pinned in the source: the accept loop needs a live listener to fail.
func TestSFTPAcceptLoopPauses(t *testing.T) {
	b, err := os.ReadFile("sftp_server.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.Contains(s, "pause = nextAcceptPause(pause)") || !strings.Contains(s, "case <-time.After(pause):") {
		t.Error("the SFTP accept loop no longer pauses after a failure")
	}
}
