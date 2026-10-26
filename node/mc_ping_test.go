package main

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestExtractMOTD(t *testing.T) {
	// string input
	if got := extractMOTD("A Minecraft Server"); got != "A Minecraft Server" {
		t.Errorf("string input: got %q", got)
	}
	// map with "text" key
	if got := extractMOTD(map[string]interface{}{"text": "My Server"}); got != "My Server" {
		t.Errorf("map with text: got %q", got)
	}
	// map without "text"
	if got := extractMOTD(map[string]interface{}{"extra": "foo"}); got != "" {
		t.Errorf("map without text: got %q", got)
	}
	// non-string/non-map type
	if got := extractMOTD(42); got != "" {
		t.Errorf("int input: got %q", got)
	}
	// nil
	if got := extractMOTD(nil); got != "" {
		t.Errorf("nil input: got %q", got)
	}
}

func TestParsePort(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"25565", 25565},
		{"19132", 19132},
		{"0", 25565},   // zero → default 25565
		{"", 25565},    // empty → default 25565
		{"abc", 25565}, // non-numeric → default 25565
	}
	for _, c := range cases {
		if got := parsePort(c.input); got != c.want {
			t.Errorf("parsePort(%q) = %d, want %d", c.input, got, c.want)
		}
	}
}

func TestVarIntRoundTrip(t *testing.T) {
	values := []int{0, 1, 127, 128, 255, 16383, 2097151}
	for _, v := range values {
		var buf bytes.Buffer
		writeVarInt(&buf, v)
		got, err := readVarInt(&buf)
		if err != nil {
			t.Fatalf("readVarInt(%d): %v", v, err)
		}
		if got != v {
			t.Errorf("VarInt roundtrip(%d) = %d", v, got)
		}
	}
}

func TestWriteReadMCString(t *testing.T) {
	cases := []string{"", "hello", "minecraft server", "unicode: 日本語"}
	for _, s := range cases {
		var buf bytes.Buffer
		writeString(&buf, s)
		got, err := readMCString(&buf)
		if err != nil {
			t.Fatalf("readMCString(%q): %v", s, err)
		}
		if got != s {
			t.Errorf("MCString roundtrip(%q) = %q", s, got)
		}
	}
}

// firstBytes accepts one connection on a loopback listener, returns the first n bytes
// the client sends, and closes (the ping then fails, which the test ignores).
func firstBytes(t *testing.T, n int, proxyLocal bool) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer c.Close()
		buf := make([]byte, n)
		io.ReadFull(c, buf)
		got <- buf
	}()
	PingMinecraftServer(ln.Addr().String(), time.Second, proxyLocal)
	return <-got
}

// The ping names its own connection in a PROXY command: Velocity drops a header
// without a source address, which is what LOCAL is.
func TestPingProxyHeader(t *testing.T) {
	got := firstBytes(t, 28, true)
	if !bytes.Equal(got[:12], proxyV2Signature) || !bytes.Equal(got[12:16], []byte{0x21, 0x11, 0x00, 12}) {
		t.Fatalf("header start = % x, want signature + PROXY TCP4 len 12", got[:16])
	}
	if !bytes.Equal(got[16:20], []byte{127, 0, 0, 1}) || !bytes.Equal(got[20:24], []byte{127, 0, 0, 1}) {
		t.Errorf("addresses = % x, want both ends 127.0.0.1", got[16:24])
	}
	// Without it the first byte is the handshake packet length (VarInt), not 0x0D.
	first := firstBytes(t, 1, false)
	if len(first) != 1 || first[0] == 0x0D || first[0] == 0 {
		t.Errorf("without proxyLocal first byte = % x, want a handshake length", first)
	}
}

func TestProxyV2Header(t *testing.T) {
	v6 := proxyV2Header(&net.TCPAddr{IP: net.ParseIP("fd00::1"), Port: 40000}, &net.TCPAddr{IP: net.ParseIP("fd00::2"), Port: 25565})
	if len(v6) != 16+36 || !bytes.Equal(v6[12:16], []byte{0x21, 0x21, 0x00, 36}) ||
		!bytes.Equal(v6[48:], []byte{0x9C, 0x40, 0x63, 0xDD}) {
		t.Errorf("v6 header = % x", v6)
	}
	if got := proxyV2Header(&net.UnixAddr{Name: "x"}, &net.UnixAddr{Name: "y"}); !bytes.Equal(got, proxyV2Local) {
		t.Errorf("non-TCP addresses = % x, want LOCAL", got)
	}
}
