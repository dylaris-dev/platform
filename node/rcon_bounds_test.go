package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// rconFlooder answers the auth handshake and then streams RESPONSE_VALUE
// packets back to back until the client hangs up, i.e. exactly what a hostile
// or merely very chatty listener on the RCON port does. That port is inside the
// tenant's own container: they choose it (rcon.port in server.properties, mirrored
// into the panel's RCON config) and they choose what listens on it.
func rconFlooder(t *testing.T, body string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, _, err := readRconPacket(conn); err != nil { // auth
			return
		}
		if err := writeRconPacket(conn, rconRequestAuth, rconTypeAuthResp, ""); err != nil {
			return
		}
		if _, _, _, err := readRconPacket(conn); err != nil { // exec
			return
		}
		for {
			if err := writeRconPacket(conn, rconRequestExec, rconTypeRespValue, body); err != nil {
				return
			}
		}
	}()
	return lis.Addr().String()
}

// Two independent bounds have to hold, and the loop had neither: it resets the
// read deadline on every packet (so it never ends while packets keep arriving)
// and appends every body to one builder (so memory grows with them).
func TestExecRconBoundsAFloodingPeer(t *testing.T) {
	addr := rconFlooder(t, strings.Repeat("A", rconMaxBodyLen))

	start := time.Now()
	out, err := execRcon(addr, "pw", "list", 500*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("execRcon returned an error rather than the reply it did receive: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("execRcon ran for %v: a peer that keeps sending never ends the read loop", elapsed)
	}
	if len(out) > rconMaxOutputLen+rconMaxBodyLen+len(rconTruncatedNote) {
		t.Errorf("execRcon assembled %d bytes; the cap is %d", len(out), rconMaxOutputLen)
	}
}

// The assembled reply crosses to Core as ONE NodeMessage, and Core's gRPC
// server refuses a received message over 128KB - which does not fail just that
// message, it ends the bidi stream and every other operation riding it.
func TestExecRconOutputFitsTheMeshMessageLimit(t *testing.T) {
	addr := rconFlooder(t, strings.Repeat("A", rconMaxBodyLen))
	out, err := execRcon(addr, "pw", "list", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	const coreMaxRecvMsgSize = 128 * 1024 // core/grpc/server.go
	if len(out) >= coreMaxRecvMsgSize {
		t.Errorf("a %d byte reply exceeds Core's %d byte receive limit and would drop the node's mesh stream",
			len(out), coreMaxRecvMsgSize)
	}
}

// The reply lands in a proto3 string field, which must be valid UTF-8 or the
// marshal fails. Truncating a stream of packets can cut a rune in half, and the
// bytes are the tenant's to choose in the first place.
func TestExecRconReturnsValidUTF8(t *testing.T) {
	// A 4096-byte body of 3-byte runes: 4096 is not divisible by 3, so every
	// packet boundary lands mid-rune.
	body := strings.Repeat("€", rconMaxBodyLen/3) + "€"[:1]
	addr := rconFlooder(t, body)

	out, err := execRcon(addr, "pw", "list", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out) {
		t.Error("execRcon returned invalid UTF-8; marshalling it into RconExecResp.Output would fail")
	}
}

// A well-behaved server answering once still returns promptly and in full: the
// bounds must not cost the ordinary case anything.
func TestExecRconStillReturnsAShortReply(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		readRconPacket(conn)
		writeRconPacket(conn, rconRequestAuth, rconTypeAuthResp, "")
		readRconPacket(conn)
		writeRconPacket(conn, rconRequestExec, rconTypeRespValue, "There are 2 of a max of 20 players online")
		io.Copy(io.Discard, conn)
	}()

	out, err := execRcon(lis.Addr().String(), "pw", "list", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out != "There are 2 of a max of 20 players online" {
		t.Errorf("got %q", out)
	}
}

// writeRawRconPacket frames a body without writeRconPacket's outbound 4096
// cap, because that cap is ours and a server is not bound by it.
func writeRawRconPacket(w io.Writer, id, typ int32, body []byte) error {
	buf := make([]byte, 4+4+4+len(body)+2)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(buf)-4))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	_, err := w.Write(buf)
	return err
}

// rconRawServer answers auth, then writes the given raw frames as the exec
// reply and keeps the connection open, as Minecraft does.
func rconRawServer(t *testing.T, frames ...[]byte) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		readRconPacket(conn)
		writeRconPacket(conn, rconRequestAuth, rconTypeAuthResp, "")
		readRconPacket(conn)
		for _, f := range frames {
			if _, err := conn.Write(f); err != nil {
				return
			}
		}
		io.Copy(io.Discard, conn)
	}()
	return lis.Addr().String()
}

func rawFrame(t *testing.T, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := writeRawRconPacket(&b, rconRequestExec, rconTypeRespValue, body); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Vanilla splits a reply every 4096 UTF-16 code units and only then encodes
// UTF-8 (MC-270327), so one packet carries up to 3*4096 body bytes. Any
// non-ASCII reply over 4096 bytes used to fail the command the server had
// already run; a broken later packet used to read as a complete reply.
func TestExecRconReadsVanillaSizedPackets(t *testing.T) {
	cyr := strings.Repeat("Ж", 4096) // 8192 bytes, one vanilla packet
	cjk := strings.Repeat("世", 4096) // 12288 bytes, the vanilla maximum
	bogus := make([]byte, 4)         // a size field no packet can have
	binary.LittleEndian.PutUint32(bogus, 1<<30)

	cases := []struct {
		name      string
		frames    [][]byte
		want      string
		wantTrunc bool
	}{
		{"two-byte runes", [][]byte{rawFrame(t, []byte(cyr))}, cyr, false},
		{"three-byte runes", [][]byte{rawFrame(t, []byte(cjk))}, cjk, false},
		{"split over two big packets", [][]byte{rawFrame(t, []byte(cjk)), rawFrame(t, []byte(cyr))}, cjk + cyr, false},
		{"broken packet after the first", [][]byte{rawFrame(t, []byte("head")), bogus}, "head", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execRcon(rconRawServer(t, tc.frames...), "pw", "x", 2*time.Second)
			if err != nil {
				t.Fatalf("execRcon: %v", err)
			}
			body, trunc := strings.CutSuffix(out, rconCutNote)
			if trunc != tc.wantTrunc {
				t.Errorf("truncation note present = %v, want %v (out tail %q)", trunc, tc.wantTrunc, out[max(0, len(out)-80):])
			}
			if body != tc.want {
				t.Errorf("got %d bytes, want %d", len(body), len(tc.want))
			}
		})
	}
}

// Core waits TimeoutMs+1s for the reply. A slow auth followed by a slow command
// reply must not stack into two (or three) full timeouts: the node would answer
// after Core already reported failure for a command that ran.
func TestExecRconHoldsOneDeadlineForTheWholeExchange(t *testing.T) {
	const timeout = 400 * time.Millisecond
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, _, err := readRconPacket(conn); err != nil {
			return
		}
		time.Sleep(timeout * 6 / 10)
		if err := writeRconPacket(conn, rconRequestAuth, rconTypeAuthResp, ""); err != nil {
			return
		}
		if _, _, _, err := readRconPacket(conn); err != nil {
			return
		}
		time.Sleep(timeout * 6 / 10)
		_ = writeRconPacket(conn, rconRequestExec, rconTypeRespValue, "late")
		time.Sleep(timeout)
	}()

	start := time.Now()
	_, _ = execRcon(lis.Addr().String(), "pw", "list", timeout)
	if elapsed := time.Since(start); elapsed > timeout+150*time.Millisecond {
		t.Errorf("execRcon ran for %v with a %v timeout: auth and exec each got their own budget", elapsed, timeout)
	}
}
