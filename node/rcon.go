package main

// Minimal RCON client. Valve-spec source-engine RCON, 4-byte LE
// header (request_id, type, length), null-terminated payload + 1 byte pad.
// We support exactly two packet types: SERVERDATA_AUTH (3) and
// SERVERDATA_EXECCOMMAND (2). Response types are SERVERDATA_AUTH_RESPONSE
// (2) and SERVERDATA_RESPONSE_VALUE (0).
//
// One connection per command — no pooling V1. RCON is request-response and
// user-driven; pooling adds complexity without saving meaningful latency at
// human-typing frequencies.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	rconTypeAuth      = 3
	rconTypeAuthResp  = 2
	rconTypeExec      = 2
	rconTypeRespValue = 0
	rconRequestExec   = 1
	rconRequestAuth   = 2
	rconMaxBodyLen    = 4096
	// rconMaxReadBodyLen bounds an INBOUND packet. Vanilla splits a reply every
	// 4096 UTF-16 code units and encodes UTF-8 afterwards (MC-270327), so one
	// packet body is up to 3*4096 bytes; 4096 rejected any non-ASCII reply.
	rconMaxReadBodyLen = 3 * rconMaxBodyLen
	rconDefaultPortVar = 25575
	rconDefaultTimeout = 3 * time.Second
	// rconIdleGap is how long to wait for a FURTHER reply packet once one has
	// arrived. Minecraft splits long replies (e.g. a big "list") across
	// packets and sends them back to back, so this only has to cover the gap
	// between them, not the server's think time - that is what the first
	// packet's full timeout is for. Too long and every command pays it; too
	// short and a split reply gets truncated.
	rconIdleGap = 250 * time.Millisecond
	// rconMaxOutputLen caps the ASSEMBLED reply. rconMaxBodyLen only bounds one
	// packet, and the loop below appends every packet it receives, so on its own
	// it bounds nothing.
	//
	// The reply crosses to Core as ONE NodeMessage, and Core's gRPC server caps
	// a received message at 128KB (core/grpc/server.go). Going over does not
	// fail just that message: RecvMsg returns ResourceExhausted, the NodeConnect
	// handler returns, and the whole bidi stream goes with it - every in-flight
	// file transfer, console attach and tab-proxy bridge on that node.
	//
	// 64KB leaves room for the envelope and is far past any real command reply
	// (a 500-player /list is under 10KB).
	rconMaxOutputLen = 64 * 1024
)

// rconTruncatedNote is appended in place of the bytes that were dropped. A
// silent cut would read as a short answer from the server, which for something
// like a player list is worse than no answer.
const rconTruncatedNote = "\n[output truncated: the server's reply exceeded the 64KB limit]"

// rconCutNote marks a reply whose connection failed after part of it arrived.
const rconCutNote = "\n[output truncated: the RCON connection failed mid-reply]"

// execRcon dials addr, auths, runs cmd, returns the server's reply.
//
// Implementation notes:
//   - MC servers may split long replies into multiple RESPONSE_VALUE packets,
//     so the reply is assembled until an idle gap rather than after one packet.
//   - The Valve end-of-reply sentinel is deliberately NOT used. This comment
//     used to describe sending one and even disagreed with the code beneath it
//     about its type; the code sent RESPONSE_VALUE(0), Minecraft rejects any
//     request that is not AUTH(3) or EXECCOMMAND(2), and closed the connection.
//     Every command failed with "read exec resp: EOF".
func execRcon(addr, password, command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = rconDefaultTimeout
	}
	// ONE deadline for dial, auth and exec. Core waits TimeoutMs+1s; separate
	// budgets per phase let a slow server answer after Core already reported a
	// command that ran as failed, and a retry runs it twice.
	hardDeadline := time.Now().Add(timeout)
	conn, err := (&net.Dialer{Deadline: hardDeadline}).Dial("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(hardDeadline)

	// --- Auth ---
	if err := writeRconPacket(conn, rconRequestAuth, rconTypeAuth, password); err != nil {
		return "", fmt.Errorf("write auth: %w", err)
	}
	id, typ, _, err := readRconPacket(conn)
	if err != nil {
		return "", fmt.Errorf("read auth resp: %w", err)
	}
	// Some implementations send an empty RESPONSE_VALUE before the AUTH_RESPONSE.
	if typ == rconTypeRespValue {
		id, typ, _, err = readRconPacket(conn)
		if err != nil {
			return "", fmt.Errorf("read auth resp (2nd): %w", err)
		}
	}
	if id == -1 {
		return "", errors.New("rcon auth failed (bad password)")
	}
	if typ != rconTypeAuthResp {
		return "", fmt.Errorf("unexpected auth response type %d", typ)
	}

	// --- Exec ---
	if err := writeRconPacket(conn, rconRequestExec, rconTypeExec, command); err != nil {
		return "", fmt.Errorf("write exec: %w", err)
	}
	// NO sentinel packet. The Valve trick - echoing back a RESPONSE_VALUE(0)
	// request to mark the end of a multi-packet reply - does not work against
	// Minecraft: its RCON accepts only AUTH(3) and EXECCOMMAND(2) as requests
	// and closes the connection on anything else. Sending it made EVERY command
	// fail with "read exec resp: EOF", which took out live player management,
	// the whitelist, operators and Scheduled Tasks "say" jobs.
	//
	// Verified against Paper 26.2 with a byte-identical client: with the
	// sentinel, EOF in ~2ms and no reply; without it, the reply arrives. It
	// looked intermittent at first because a slower client can win the race and
	// receive the reply before the server acts on the sentinel - so "it worked
	// once" is not evidence here.
	//
	// End-of-reply is instead detected by an idle gap: the first packet gets the
	// caller's full timeout, and every packet after it a short one, so a reply
	// split across packets is still assembled while a single-packet reply does
	// not cost the whole timeout.
	//
	// Both bounds below exist because what answers on the RCON port is inside
	// the tenant's own container - they pick the port (rcon.port in
	// server.properties) and they pick what listens on it.
	//
	// hardDeadline bounds the whole exchange. The per-packet SetReadDeadline
	// calls OVERRIDE the connection deadline set after the dial, so without the
	// cap a peer sending one packet every rconIdleGap keeps this goroutine, and
	// the buffer it is filling, alive indefinitely.
	var out strings.Builder
	got := false
	truncated := false
	cut := false
	for {
		deadline := time.Now().Add(timeout)
		if got {
			deadline = time.Now().Add(rconIdleGap)
		}
		if deadline.After(hardDeadline) {
			deadline = hardDeadline
		}
		_ = conn.SetReadDeadline(deadline)
		rid, rtyp, body, err := readRconPacket(conn)
		if err != nil {
			// Once something arrived, a read timeout (or a close at a packet
			// boundary) is the end of the reply, not a failure. Anything else
			// cut the reply, and must not read as a complete one.
			if got {
				var ne net.Error
				if !(errors.As(err, &ne) && ne.Timeout()) && !errors.Is(err, io.EOF) {
					cut = true
				}
				break
			}
			return "", fmt.Errorf("read exec resp: %w", err)
		}
		if rtyp != rconTypeRespValue || rid != rconRequestExec {
			continue // not our reply
		}
		got = true
		out.WriteString(body)
		if out.Len() >= rconMaxOutputLen {
			truncated = true
			break
		}
	}
	// Stopping mid-stream can cut a multi-byte rune in half, and the bytes were
	// the tenant's to choose regardless. Output is a proto3 string field, which
	// must be valid UTF-8 or the marshal fails - and a marshal failure on this
	// message is a failure of the stream carrying it.
	res := strings.ToValidUTF8(strings.TrimRight(out.String(), "\x00 \r\n"), "")
	if truncated {
		res += rconTruncatedNote
	} else if cut {
		res += rconCutNote
	}
	return res, nil
}

func writeRconPacket(w io.Writer, id int32, typ int32, body string) error {
	if len(body) > rconMaxBodyLen {
		return fmt.Errorf("body too large (%d)", len(body))
	}
	// payload: id(4) + type(4) + body + 1 null + 1 trailing null = body+10
	payloadLen := int32(4 + 4 + len(body) + 2)
	buf := make([]byte, 4+payloadLen) // +4 for size header
	binary.LittleEndian.PutUint32(buf[0:4], uint32(payloadLen))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	// Last two bytes already 0 (Go default).
	_, err := w.Write(buf)
	return err
}

func readRconPacket(r io.Reader) (int32, int32, string, error) {
	var sizeBuf [4]byte
	if _, err := io.ReadFull(r, sizeBuf[:]); err != nil {
		return 0, 0, "", err
	}
	size := int32(binary.LittleEndian.Uint32(sizeBuf[:]))
	if size < 10 || size > rconMaxReadBodyLen+10 {
		return 0, 0, "", fmt.Errorf("invalid packet size %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, 0, "", err
	}
	id := int32(binary.LittleEndian.Uint32(buf[0:4]))
	typ := int32(binary.LittleEndian.Uint32(buf[4:8]))
	// Body is everything between offset 8 and the trailing two null bytes.
	body := string(buf[8 : size-2])
	return id, typ, body, nil
}
