package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeSLP answers a server-list ping like a Minecraft server: with expectProxy
// it requires a PROXY v2 header first and drops a connection without one,
// without it it drops a connection that starts with one - which is what Paper
// does with proxy-protocol on and off.
func fakeSLP(t *testing.T, expectProxy bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				first := make([]byte, 1)
				if _, err := io.ReadFull(c, first); err != nil {
					return
				}
				gotProxy := first[0] == proxyV2Signature[0]
				if gotProxy != expectProxy {
					return
				}
				if gotProxy {
					head := make([]byte, 15)
					if _, err := io.ReadFull(c, head); err != nil || !bytes.Equal(append(first, head[:11]...), proxyV2Signature) {
						return
					}
					if _, err := io.ReadFull(c, make([]byte, int(head[13])<<8|int(head[14]))); err != nil {
						return
					}
					first = nil
				}
				conn := &prefixConn{Conn: c, prefix: first}
				if _, err := readPacket(conn); err != nil { // handshake
					return
				}
				if _, err := readPacket(conn); err != nil { // status request
					return
				}
				var body bytes.Buffer
				body.WriteByte(0x00)
				writeString(&body, `{"version":{"name":"fake"},"players":{"online":3,"max":20},"description":"hi"}`)
				writePacket(c, body.Bytes())
			}(c)
		}
	}()
	return ln.Addr().String()
}

// prefixConn replays bytes already read before the rest of the stream.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// Whatever the server expects, the two-way ping gets an answer and says which
// way worked - from either starting guess. That is the detection Core's Auto
// mode rests on.
func TestPingEitherWayFindsWhatTheServerExpects(t *testing.T) {
	for _, expect := range []bool{false, true} {
		addr := fakeSLP(t, expect)
		for _, prefer := range []bool{false, true} {
			resp, withProxy, err := pingEitherWay(func(proxy bool) (*SLPResponse, error) {
				return PingMinecraftServer(addr, time.Second, proxy)
			}, prefer)
			if err != nil || resp == nil || resp.Players != 3 {
				t.Fatalf("expect=%v prefer=%v: %+v, %v", expect, prefer, resp, err)
			}
			if withProxy != expect {
				t.Fatalf("expect=%v prefer=%v: reported withProxy=%v", expect, prefer, withProxy)
			}
		}
	}
}

func TestPingEitherWayReportsAServerThatAnswersNeither(t *testing.T) {
	calls := 0
	_, _, err := pingEitherWay(func(bool) (*SLPResponse, error) { calls++; return nil, errors.New("refused") }, true)
	if err == nil || calls != 2 {
		t.Fatalf("err %v after %d tries, want an error after 2", err, calls)
	}
}

func TestProxyReadyReporterWritesOnChangeAndRefresh(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	var p proxyReadyReporter
	t0 := time.Unix(1_000_000, 0)

	p.report(ctx, rdb, "u", true, t0)
	if v, _ := mr.Get(proxyReadyKey("u")); v != "1" {
		t.Fatalf("first answer not written: %q", v)
	}
	mr.Del(proxyReadyKey("u"))
	p.report(ctx, rdb, "u", true, t0.Add(30*time.Second))
	if mr.Exists(proxyReadyKey("u")) {
		t.Fatal("the same answer was written again inside the refresh interval")
	}
	p.report(ctx, rdb, "u", false, t0.Add(40*time.Second))
	if v, _ := mr.Get(proxyReadyKey("u")); v != "0" {
		t.Fatalf("a changed answer was not written at once: %q", v)
	}
	mr.Del(proxyReadyKey("u"))
	p.report(ctx, rdb, "u", false, t0.Add(40*time.Second+proxyReadyRefresh))
	if v, _ := mr.Get(proxyReadyKey("u")); v != "0" {
		t.Fatalf("not refreshed after the interval: %q", v)
	}
	if ttl := mr.TTL(proxyReadyKey("u")); ttl <= 0 || ttl > proxyReadyTTL {
		t.Fatalf("ttl %v", ttl)
	}
}
