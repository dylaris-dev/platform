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

	// A customer arriving now still gets in, and a stalled one goes.
	cust := &closeRecorder{}
	if !s.admit(cust, "192.0.2.7") {
		t.Fatal("a customer was refused while strangers held stalled handshakes")
	}
	after := 0
	for _, c := range stalled {
		if !c.closed {
			after++
		}
	}
	if cust.closed || after != open-1 {
		t.Fatalf("customer closed=%v, stalled dropped=%d; want false, 1", cust.closed, open-after)
	}
}

// A few addresses that keep the pre-auth budget full must not be able to cycle
// a real handshake out of it: eviction takes from whoever holds the most, so a
// customer with one handshake outlives any number of stalled arrivals. An IPv6
// attacker counts as one holder per /48, since one subscriber holds hundreds of
// /64s.
func TestSFTPPreAuthEvictsTheBiggestHolder(t *testing.T) {
	cases := []struct {
		name     string
		attacker func(i int) string
		customer string
	}{
		{"8 IPv4 addresses", func(i int) string { return fmt.Sprintf("198.51.%d.1", i%8) }, "192.0.2.7"},
		{"32 IPv6 /64s of one /48", func(i int) string { return fmt.Sprintf("2001:db8:1:%x::/64", i%32) }, "2001:db8:2::/64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSFTPServer(nil, nil, "node-1")
			var stalled []*closeRecorder
			reap := func() {
				for _, c := range stalled {
					if c.closed && !c.released {
						c.released = true
						s.handshakeDone(c)
						s.release(c.addr)
					}
				}
			}
			flood := func(n int) {
				for i := 0; i < n; i++ {
					c := &closeRecorder{addr: tc.attacker(i)}
					if s.admit(c, c.addr) {
						stalled = append(stalled, c)
					}
					reap()
				}
			}
			flood(sftpMaxPreAuth * 2)
			cust := &closeRecorder{}
			if !s.admit(cust, tc.customer) {
				t.Fatal("customer refused")
			}
			flood(sftpMaxPreAuth * 20)
			if cust.closed {
				t.Fatal("the attacker's stalled handshakes evicted the customer's")
			}
		})
	}
}
