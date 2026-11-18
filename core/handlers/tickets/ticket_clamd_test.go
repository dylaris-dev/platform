package tickets

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

// fakeClamd accepts one INSTREAM, drains it up to the zero-length chunk and
// answers with reply (written raw, so a test can send nothing at all).
func fakeClamd(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		cmd := make([]byte, len("zINSTREAM\x00"))
		if _, err := io.ReadFull(c, cmd); err != nil {
			return
		}
		for {
			var sz [4]byte
			if _, err := io.ReadFull(c, sz[:]); err != nil {
				return
			}
			n := int(sz[0])<<24 | int(sz[1])<<16 | int(sz[2])<<8 | int(sz[3])
			if n == 0 {
				break
			}
			if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
				return
			}
		}
		_, _ = c.Write([]byte(reply))
	}()
	return ln.Addr().String()
}

// Scan is fail-closed: only clamd's explicit "stream: OK" is clean. An
// ERROR reply (size limit), or no reply at all, used to read as clean.
func TestClamdScanIsFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantErr bool
		wantHit bool
	}{
		{"clean", "stream: OK\x00", false, false},
		{"virus", "stream: Eicar-Signature FOUND\x00", true, true},
		{"size limit", "INSTREAM size limit exceeded. ERROR\x00", true, false},
		{"empty reply", "", true, false},
		{"garbage", "PONG\x00", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &clamdScanner{addr: fakeClamd(t, c.reply)}
			err := s.Scan(strings.NewReader("payload"))
			if (err != nil) != c.wantErr {
				t.Fatalf("Scan() err = %v, wantErr %v", err, c.wantErr)
			}
			if errors.Is(err, errScanHit) != c.wantHit {
				t.Errorf("errors.Is(err, errScanHit) = %v, want %v (err %v)", !c.wantHit, c.wantHit, err)
			}
		})
	}
}
