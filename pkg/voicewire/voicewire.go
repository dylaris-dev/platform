// CROSS-REPO: platform/pkg/voicewire and gateway/pkg/voicewire hold byte-identical
// copies of this file. The gateway voice relay and the platform node forwarder
// each compile their own, so this is a frozen wire contract: never change the
// layout of a version in place. A change bumps Version in BOTH repos in the same release.

// Package voicewire is the UDP frame between the gateway voice relay and the
// node forwarder that carries proximity voice chat (Simple Voice Chat, Plasmo
// Voice) to a server's container.
//
// Every frame starts with a 20-byte header, integers big-endian:
//
//	[0xD7 'V'][ver=2][type][server_id:16]
//
// followed by, per type:
//
//	REG       (node->relay)  [unix_ms:8][nonce:8][mac:16]  52 bytes
//	REG_ACK   (relay->node)  [unix_ms:8]                   28 bytes
//	DATA_UP   (relay->node)  [session:4][payload...]       24+ bytes
//	DATA_DOWN (node->relay)  [session:4][payload...]       24+ bytes
//	CLOSE     (either)       [session:4]                   24 bytes
//
// mac is the first 16 bytes of HMAC-SHA256(key, the first 36 bytes of the REG).
//
// server_id is ServerID(server uuid). Version 1 carried the uuid itself and
// assumed the RFC 8-4-4-4-12 form, which no Dylaris server uuid has
// ("<owner>_<random>"); it never carried a packet and is refused.
package voicewire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const (
	Version byte = 2

	TypeReg      byte = 1
	TypeRegAck   byte = 2
	TypeDataUp   byte = 3 // relay to node
	TypeDataDown byte = 4 // node to relay
	TypeClose    byte = 5

	HeaderLen    = 20
	RegLen       = 52
	RegAckLen    = 28
	DataOverhead = 24
	CloseLen     = 24

	magic0 byte = 0xD7
	magic1 byte = 'V'

	regMACOffset = 36
	macLen       = 16
)

var (
	ErrShort   = errors.New("voicewire: frame shorter than header")
	ErrMagic   = errors.New("voicewire: bad magic")
	ErrVersion = errors.New("voicewire: unknown version")
	ErrType    = errors.New("voicewire: unknown type")
	ErrLength  = errors.New("voicewire: wrong length for type")
)

// Frame is a parsed frame. Only the fields of its Type are set.
type Frame struct {
	Type    byte
	Server  [16]byte
	UnixMs  uint64
	Nonce   [8]byte
	MAC     [16]byte
	Session uint32
	Payload []byte // aliases the parsed buffer
}

func appendHeader(dst []byte, typ byte, server [16]byte) []byte {
	dst = append(dst, magic0, magic1, Version, typ)
	return append(dst, server[:]...)
}

func regMAC(key, signed []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(signed)
	return m.Sum(nil)[:macLen]
}

// AppendReg appends a REG frame signed with key.
func AppendReg(dst []byte, server [16]byte, unixMs uint64, nonce [8]byte, key []byte) []byte {
	start := len(dst)
	dst = appendHeader(dst, TypeReg, server)
	dst = binary.BigEndian.AppendUint64(dst, unixMs)
	dst = append(dst, nonce[:]...)
	return append(dst, regMAC(key, dst[start:start+regMACOffset])...)
}

// AppendRegAck appends a REG_ACK frame echoing the REG's unix_ms.
func AppendRegAck(dst []byte, server [16]byte, unixMs uint64) []byte {
	dst = appendHeader(dst, TypeRegAck, server)
	return binary.BigEndian.AppendUint64(dst, unixMs)
}

// AppendData appends a DATA_UP or DATA_DOWN frame. Any other typ is a
// programming error and panics.
func AppendData(dst []byte, typ byte, server [16]byte, session uint32, payload []byte) []byte {
	if typ != TypeDataUp && typ != TypeDataDown {
		panic("voicewire: AppendData with a non-DATA type")
	}
	dst = appendHeader(dst, typ, server)
	dst = binary.BigEndian.AppendUint32(dst, session)
	return append(dst, payload...)
}

// AppendClose appends a CLOSE frame.
func AppendClose(dst []byte, server [16]byte, session uint32) []byte {
	dst = appendHeader(dst, TypeClose, server)
	return binary.BigEndian.AppendUint32(dst, session)
}

// Parse decodes one frame. REG, REG_ACK and CLOSE must be exactly their
// length; DATA must be at least DataOverhead. It does not verify a REG's MAC,
// see VerifyReg.
func Parse(b []byte) (Frame, error) {
	var f Frame
	if len(b) < HeaderLen {
		return f, ErrShort
	}
	if b[0] != magic0 || b[1] != magic1 {
		return f, ErrMagic
	}
	if b[2] != Version {
		return f, ErrVersion
	}
	f.Type = b[3]
	copy(f.Server[:], b[4:HeaderLen])
	switch f.Type {
	case TypeReg:
		if len(b) != RegLen {
			return f, ErrLength
		}
		f.UnixMs = binary.BigEndian.Uint64(b[20:28])
		copy(f.Nonce[:], b[28:36])
		copy(f.MAC[:], b[regMACOffset:RegLen])
	case TypeRegAck:
		if len(b) != RegAckLen {
			return f, ErrLength
		}
		f.UnixMs = binary.BigEndian.Uint64(b[20:28])
	case TypeDataUp, TypeDataDown:
		if len(b) < DataOverhead {
			return f, ErrLength
		}
		f.Session = binary.BigEndian.Uint32(b[20:24])
		f.Payload = b[DataOverhead:]
	case TypeClose:
		if len(b) != CloseLen {
			return f, ErrLength
		}
		f.Session = binary.BigEndian.Uint32(b[20:24])
	default:
		return f, ErrType
	}
	return f, nil
}

// VerifyReg reports whether raw is a well-formed REG whose MAC matches key,
// compared in constant time.
func VerifyReg(raw []byte, key []byte) bool {
	f, err := Parse(raw)
	if err != nil || f.Type != TypeReg {
		return false
	}
	return hmac.Equal(f.MAC[:], regMAC(key, raw[:regMACOffset]))
}

// ServerID is the 16-byte server field of a frame: the first 16 bytes of
// SHA-256 over the server uuid. It is one-way on purpose; whoever needs the uuid
// back (the relay's metering) keeps it beside the id.
func ServerID(serverUUID string) [16]byte {
	sum := sha256.Sum256([]byte(serverUUID))
	var b [16]byte
	copy(b[:], sum[:16])
	return b
}
