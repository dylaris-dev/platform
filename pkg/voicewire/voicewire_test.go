// CROSS-REPO: platform/pkg/voicewire and gateway/pkg/voicewire hold byte-identical
// copies of this file.
// Both repos pin the same literal goldens, so a format change on one side turns
// that side red instead of breaking the other at runtime.

package voicewire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

var (
	testKey = func() []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = byte(i)
		}
		return k
	}()
	testServer = [16]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	testNonce  = [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
)

const (
	testUnixMs  uint64 = 1700000000000
	testSession uint32 = 0x0a0b0c0d
)

func TestTheWireFormatIsFrozen(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want string
		len  int
	}{
		{"REG", AppendReg(nil, testServer, testUnixMs, testNonce, testKey),
			"d756010100112233445566778899aabbccddeeff0000018bcfe56800010203040506070869fef3a9c3e1b0dfbd9fe7671b749385", RegLen},
		{"REG_ACK", AppendRegAck(nil, testServer, testUnixMs),
			"d756010200112233445566778899aabbccddeeff0000018bcfe56800", RegAckLen},
		{"DATA_UP", AppendData(nil, TypeDataUp, testServer, testSession, []byte("hi")),
			"d756010300112233445566778899aabbccddeeff0a0b0c0d6869", DataOverhead + 2},
		{"DATA_DOWN", AppendData(nil, TypeDataDown, testServer, testSession, []byte("hi")),
			"d756010400112233445566778899aabbccddeeff0a0b0c0d6869", DataOverhead + 2},
		{"CLOSE", AppendClose(nil, testServer, testSession),
			"d756010500112233445566778899aabbccddeeff0a0b0c0d", CloseLen},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(c.got); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
		if len(c.got) != c.len {
			t.Errorf("%s: length %d, want %d", c.name, len(c.got), c.len)
		}
	}
}

func TestParseRoundTripsEveryType(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want Frame
	}{
		{"REG", AppendReg(nil, testServer, testUnixMs, testNonce, testKey),
			Frame{Type: TypeReg, Server: testServer, UnixMs: testUnixMs, Nonce: testNonce}},
		{"REG_ACK", AppendRegAck(nil, testServer, testUnixMs),
			Frame{Type: TypeRegAck, Server: testServer, UnixMs: testUnixMs}},
		{"DATA_UP", AppendData(nil, TypeDataUp, testServer, testSession, []byte("hi")),
			Frame{Type: TypeDataUp, Server: testServer, Session: testSession, Payload: []byte("hi")}},
		{"DATA_DOWN with empty payload", AppendData(nil, TypeDataDown, testServer, testSession, nil),
			Frame{Type: TypeDataDown, Server: testServer, Session: testSession}},
		{"CLOSE", AppendClose(nil, testServer, testSession),
			Frame{Type: TypeClose, Server: testServer, Session: testSession}},
	}
	for _, c := range cases {
		f, err := Parse(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if f.Type != c.want.Type || f.Server != c.want.Server || f.UnixMs != c.want.UnixMs ||
			f.Nonce != c.want.Nonce || f.Session != c.want.Session || !bytes.Equal(f.Payload, c.want.Payload) {
			t.Errorf("%s: got %+v, want %+v", c.name, f, c.want)
		}
		if c.want.Type == TypeReg && f.MAC != [16]byte(c.raw[36:52]) {
			t.Errorf("%s: MAC not parsed", c.name)
		}
	}
}

func TestParseRefusesUnknownVersionAndType(t *testing.T) {
	base := AppendClose(nil, testServer, testSession)
	mutate := func(i int, v byte) []byte { b := bytes.Clone(base); b[i] = v; return b }
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"version 2", mutate(2, 2), ErrVersion},
		{"type 6", mutate(3, 6), ErrType},
		{"type 0", mutate(3, 0), ErrType},
		{"magic d7 57", mutate(1, 0x57), ErrMagic},
		{"shorter than a header", base[:HeaderLen-1], ErrShort},
	}
	for _, c := range cases {
		if _, err := Parse(c.raw); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func TestParseRefusesWrongFixedLengths(t *testing.T) {
	reg := AppendReg(nil, testServer, testUnixMs, testNonce, testKey)
	closeFrame := AppendClose(nil, testServer, testSession)
	data := AppendData(nil, TypeDataUp, testServer, testSession, nil)
	cases := []struct {
		name string
		raw  []byte
	}{
		{"REG of 51 bytes", reg[:RegLen-1]},
		{"REG of 53 bytes", append(bytes.Clone(reg), 0)},
		{"REG_ACK of 29 bytes", append(AppendRegAck(nil, testServer, testUnixMs), 0)},
		{"CLOSE of 25 bytes", append(bytes.Clone(closeFrame), 0)},
		{"DATA of 23 bytes", data[:DataOverhead-1]},
	}
	for _, c := range cases {
		if _, err := Parse(c.raw); !errors.Is(err, ErrLength) {
			t.Errorf("%s: got %v, want ErrLength", c.name, err)
		}
	}
}

func TestVerifyRegRejectsAFlippedBit(t *testing.T) {
	reg := AppendReg(nil, testServer, testUnixMs, testNonce, testKey)
	if !VerifyReg(reg, testKey) {
		t.Fatal("an untouched REG must verify")
	}
	for _, c := range []struct {
		name string
		i    int
	}{{"server", 4}, {"unix_ms", 27}, {"nonce", 30}, {"MAC", 40}} {
		b := bytes.Clone(reg)
		b[c.i] ^= 0x01
		if VerifyReg(b, testKey) {
			t.Errorf("a flipped bit in %s still verifies", c.name)
		}
	}
	if VerifyReg(reg, testKey[:31]) {
		t.Error("a different key still verifies")
	}
	if VerifyReg(AppendClose(nil, testServer, testSession), testKey) {
		t.Error("a frame that is not a REG verifies")
	}
}

func TestRegAckIsSmallerThanReg(t *testing.T) {
	// A spoofed REG must never earn a bigger reply: that would make the
	// relay an amplifier.
	if RegAckLen >= RegLen {
		t.Fatalf("RegAckLen %d is not smaller than RegLen %d", RegAckLen, RegLen)
	}
}

func TestServerUUIDRoundTrips(t *testing.T) {
	const s = "00112233-4455-6677-8899-aabbccddeeff"
	b, err := ParseServerUUID(s)
	if err != nil || b != testServer {
		t.Fatalf("ParseServerUUID(%q) = %x, %v", s, b, err)
	}
	if got := FormatServerUUID(b); got != s {
		t.Errorf("FormatServerUUID = %q, want %q", got, s)
	}
	for _, bad := range []string{"", "00112233445566778899aabbccddeeff", "00112233-4455-6677-8899-aabbccddeefg", "00112233-4455-6677-8899_aabbccddeeff"} {
		if _, err := ParseServerUUID(bad); err == nil {
			t.Errorf("ParseServerUUID(%q) accepted", bad)
		}
	}
}
