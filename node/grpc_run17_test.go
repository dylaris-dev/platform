package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	pb "dylaris-proto/node"

	"github.com/redis/go-redis/v9"
)

// The mesh upload budget runs on the shared per-Core read loop. A Redis that
// accepts and never answers held that loop for the client's whole read
// timeout and retries, and with it every flow credit and WsFrame from Core.
func TestTheMeshUploadBudgetDoesNotWaitOutAHungRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Refuses the handshake (the client falls back and the connection
			// is up) and then never answers a GET: a Redis that hangs on an
			// established connection, which the client retries.
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch strings.ToUpper(strings.TrimSpace(l)) {
					case "GET":
						<-done
						return
					case "HELLO", "CLIENT", "PING":
						c.Write([]byte("-ERR unknown\r\n"))
					}
				}
			}(c)
		}
	}()
	// go-redis defaults, as newNodeRedisClient keeps them: a 3s read timeout
	// and three retries, so ~12s per GET unbounded. The client does not set
	// ContextTimeoutEnabled, so the deadline cannot cut the first read short;
	// it stops the retries.
	rdb := redis.NewClient(&redis.Options{Addr: ln.Addr().String()})
	defer rdb.Close()

	start := time.Now()
	b, ok := uploadBudget(rdb, t.TempDir(), "srv-hung", 1)
	b.release()
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("uploadBudget took %v against a hung Redis", took)
	}
	if !ok {
		t.Fatal("a failed gauge read must read as no known limit")
	}
}

// A reply boundForCore replaced with an error ends the request on Core. The
// sender must hear about it, or a tab proxy streams the body into a request
// Core has already closed.
func TestSendReportsAReplacedReply(t *testing.T) {
	cases := []struct {
		name     string
		msg      *pb.NodeMessage
		replaced bool
	}{
		{"within the limit", &pb.NodeMessage{RequestId: "r1", Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: []byte("x")}}}, false},
		{"over the limit", &pb.NodeMessage{RequestId: "r2", Payload: &pb.NodeMessage_HttpProxyRespHead{HttpProxyRespHead: &pb.HttpProxyRespHead{
			StatusCode: 200, Headers: []*pb.HttpHeader{{Key: "X-Big", Value: strings.Repeat("x", 200<<10)}},
		}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeCoreStream{}
			cc := &coreConnection{stream: fake}
			err := cc.send(c.msg)
			if got := errors.Is(err, errReplyReplaced); got != c.replaced {
				t.Fatalf("send err = %v, want replaced=%v", err, c.replaced)
			}
			if !c.replaced && err != nil {
				t.Fatalf("send err = %v", err)
			}
			msgs := fake.messages()
			if len(msgs) != 1 || (msgs[0].GetError() != nil) != c.replaced {
				t.Fatalf("Core got %v", msgs)
			}
		})
	}
}

// End to end: an oversized response head stops the body stream.
func TestAReplacedProxyHeadStopsTheBody(t *testing.T) {
	fake := &fakeCoreStream{}
	cc := &coreConnection{stream: fake}
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"X-Big": {strings.Repeat("x", 200<<10)}},
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("b", 4*chunkSize))),
	}
	(&StreamHandler{}).streamProxyResponse("r", resp, cc.send)
	msgs := fake.messages()
	if len(msgs) != 1 || msgs[0].GetError() == nil {
		t.Fatalf("Core got %d messages, want only the error", len(msgs))
	}
}
