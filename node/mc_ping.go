package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// SLPResponse holds the parsed Server List Ping response.
type SLPResponse struct {
	Players    int    `json:"players"`
	MaxPlayers int    `json:"maxPlayers"`
	MOTD       string `json:"motd"`
	Version    string `json:"version"`
}

// proxyV2Signature opens every HAProxy PROXY protocol v2 header.
var proxyV2Signature = []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

// proxyV2Local is the v2 header with command LOCAL, AF_UNSPEC and no address block
// (spec section 2.2, "health check, no client address").
var proxyV2Local = append(append([]byte{}, proxyV2Signature...), 0x20, 0x00, 0x00, 0x00)

// proxyV2Header is the header the ping sends to a server that expects PROXY protocol.
// It is a PROXY command naming the node's own end of the connection, not LOCAL:
// Velocity reads the source address of every header it gets and drops a connection
// whose header carries none (MinecraftConnection.channelRead builds an
// InetSocketAddress from it), so a LOCAL ping never reached a Velocity proxy and
// every such proxy sat on "starting" until readinessBound. Paper and BungeeCord
// accept either. LOCAL stays for an address that is not TCP, which a "tcp" dial
// does not produce.
func proxyV2Header(local, remote net.Addr) []byte {
	src, ok1 := local.(*net.TCPAddr)
	dst, ok2 := remote.(*net.TCPAddr)
	if !ok1 || !ok2 {
		return proxyV2Local
	}
	h := append([]byte{}, proxyV2Signature...)
	if s4, d4 := src.IP.To4(), dst.IP.To4(); s4 != nil && d4 != nil {
		h = append(h, 0x21, 0x11, 0x00, 12)
		h = append(h, s4...)
		h = append(h, d4...)
	} else {
		h = append(h, 0x21, 0x21, 0x00, 36)
		h = append(h, src.IP.To16()...)
		h = append(h, dst.IP.To16()...)
	}
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(h, uint16(src.Port)), uint16(dst.Port))
}

// PingMinecraftServer performs an SLP (Server List Ping) on the given address.
// address should be host:port (e.g. "mc_<uuid>:25565"). proxyLocal prefixes a PROXY v2
// header (proxyV2Header), needed when the server expects PROXY protocol from the gateway.
func PingMinecraftServer(address string, timeout time.Duration, proxyLocal bool) (*SLPResponse, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, fmt.Errorf("connect: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	if proxyLocal {
		if _, err := conn.Write(proxyV2Header(conn.LocalAddr(), conn.RemoteAddr())); err != nil {
			return nil, fmt.Errorf("send proxy header: %v", err)
		}
	}

	host, port, _ := net.SplitHostPort(address)

	// Build handshake packet
	var handshake bytes.Buffer
	handshake.WriteByte(0x00)    // Packet ID: Handshake
	writeVarInt(&handshake, 764) // Protocol version (1.20.2, widely compatible)
	writeString(&handshake, host)
	binary.Write(&handshake, binary.BigEndian, uint16(parsePort(port)))
	writeVarInt(&handshake, 1) // Next state: Status

	// Send handshake
	if err := writePacket(conn, handshake.Bytes()); err != nil {
		return nil, fmt.Errorf("send handshake: %v", err)
	}

	// Send status request (empty packet with ID 0x00)
	if err := writePacket(conn, []byte{0x00}); err != nil {
		return nil, fmt.Errorf("send status request: %v", err)
	}

	// Read response
	data, err := readPacket(conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %v", err)
	}

	// Skip packet ID (0x00)
	buf := bytes.NewReader(data)
	_, _ = readVarInt(buf) // packet ID

	// Read JSON string
	jsonStr, err := readMCString(buf)
	if err != nil {
		return nil, fmt.Errorf("read json: %v", err)
	}

	// Parse JSON
	var resp struct {
		Description interface{} `json:"description"`
		Players     struct {
			Max    int `json:"max"`
			Online int `json:"online"`
		} `json:"players"`
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, fmt.Errorf("parse json: %v", err)
	}

	motd := extractMOTD(resp.Description)

	return &SLPResponse{
		Players:    resp.Players.Online,
		MaxPlayers: resp.Players.Max,
		MOTD:       motd,
		Version:    resp.Version.Name,
	}, nil
}

func extractMOTD(desc interface{}) string {
	switch v := desc.(type) {
	case string:
		return v
	case map[string]interface{}:
		if text, ok := v["text"].(string); ok {
			return text
		}
	}
	return ""
}

func parsePort(s string) int {
	var port int
	fmt.Sscanf(s, "%d", &port)
	if port == 0 {
		return 25565
	}
	return port
}

// VarInt encoding/decoding

func writeVarInt(w *bytes.Buffer, value int) {
	for {
		b := byte(value & 0x7F)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		w.WriteByte(b)
		if value == 0 {
			break
		}
	}
}

func readVarInt(r io.ByteReader) (int, error) {
	var result int
	var shift uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= int(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 35 {
			return 0, fmt.Errorf("varint too big")
		}
	}
	return result, nil
}

func writeString(w *bytes.Buffer, s string) {
	writeVarInt(w, len(s))
	w.WriteString(s)
}

func readMCString(r io.ByteReader) (string, error) {
	length, err := readVarInt(r)
	if err != nil {
		return "", err
	}
	if length < 0 || length > 32767*4 {
		return "", fmt.Errorf("string too long: %d", length)
	}
	buf := make([]byte, length)
	for i := 0; i < length; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		buf[i] = b
	}
	return string(buf), nil
}

func writePacket(conn net.Conn, data []byte) error {
	var lenBuf bytes.Buffer
	writeVarInt(&lenBuf, len(data))
	if _, err := conn.Write(lenBuf.Bytes()); err != nil {
		return err
	}
	_, err := conn.Write(data)
	return err
}

func readPacket(conn net.Conn) ([]byte, error) {
	// Read packet length as VarInt
	var length int
	var shift uint
	for {
		var b [1]byte
		if _, err := conn.Read(b[:]); err != nil {
			return nil, err
		}
		length |= int(b[0]&0x7F) << shift
		if b[0]&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 35 {
			return nil, fmt.Errorf("packet length varint too big")
		}
	}

	if length <= 0 || length > 1024*1024 {
		return nil, fmt.Errorf("invalid packet length: %d", length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}
	return data, nil
}
