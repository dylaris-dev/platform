package main

import (
	"fmt"
	"net"
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

type closeRecorder struct {
	net.Conn
	addr     string
	closed   bool
	released bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// Connections that never finish the SSH handshake must not be able to hold
// the node's connection slots: they share a small pre-auth budget in which the
// oldest is dropped for a new arrival, and a finished handshake leaves it.
func TestSFTPPreAuthBudget(t *testing.T) {
	s := NewSFTPServer(nil, nil, "node-1")

	// Signed-in sessions from many addresses.
	for i := 0; i < 200; i++ {
		c := &closeRecorder{}
		if !s.admit(c, fmt.Sprintf("203.0.113.%d", i)) {
			t.Fatalf("session %d refused", i)
		}
		s.handshakeDone(c)
	}

	// A stranger with 16 addresses opens 16 stalled handshakes from each.
	var stalled []*closeRecorder
	// What a dropped connection's goroutine does once its handshake fails.
	reap := func() {
		for _, c := range stalled {
			if c.closed && !c.released {
				c.released = true
				s.handshakeDone(c)
				s.release(c.addr)
			}
		}
	}
	for a := 0; a < 16; a++ {
		for j := 0; j < sftpMaxConnsPerAddr; j++ {
			c := &closeRecorder{addr: fmt.Sprintf("198.51.100.%d", a)}
			if s.admit(c, c.addr) {
				stalled = append(stalled, c)
			}
			reap()
		}
	}
	open, perAddr := 0, map[string]int{}
	for _, c := range stalled {
		if !c.closed {
			open++
			perAddr[c.addr]++
		}
	}
	if open > sftpMaxPreAuth {
		t.Fatalf("%d stalled handshakes hold a slot, want at most %d", open, sftpMaxPreAuth)
	}
	for a, n := range perAddr {
		if n > sftpMaxPreAuthPerAddr {
			t.Fatalf("%s holds %d stalled handshakes, want at most %d", a, n, sftpMaxPreAuthPerAddr)
		}
	}

	// A customer arriving now still gets in, and the oldest stalled one goes.
	var oldest *closeRecorder
	for _, c := range stalled {
		if !c.closed {
			oldest = c
			break
		}
	}
	cust := &closeRecorder{}
	if !s.admit(cust, "192.0.2.7") {
		t.Fatal("a customer was refused while strangers held stalled handshakes")
	}
	if cust.closed || oldest == nil || !oldest.closed {
		t.Fatalf("customer closed=%v, oldest stalled dropped=%v; want false, true", cust.closed, oldest != nil && oldest.closed)
	}
}
