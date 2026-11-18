package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "dylaris-proto/node"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Y01: a mesh upload looked the server path up (a Redis GET each) about six
// times on the shared per-Core read loop. It resolves it once, at WriteReq.
func TestAMeshUploadResolvesTheServerPathOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	base := t.TempDir()
	sm := NewStorageManager(base, rdb)
	if err := os.MkdirAll(filepath.Join(base, run18UUID, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	mr.Set("node:"+nodeID+":server:"+run18UUID+":storage", base)
	m := &MeshManager{handler: NewStreamHandler(sm), pendingWrites: map[string]*pendingWrite{}}
	cc := &coreConnection{stream: &fakeCoreStream{}}
	sm.GetServerPath(run18UUID) // the connection handshake is not a lookup

	before := mr.CommandCount()
	m.handleRequest(cc, &pb.NodeMessage{RequestId: "u", ServerUuid: run18UUID, Payload: &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: "survival/a.txt", TotalSize: 3}}})
	m.handleRequest(cc, &pb.NodeMessage{RequestId: "u", Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: []byte("abc")}}})
	m.handleRequest(cc, &pb.NodeMessage{RequestId: "u", Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: 3}}})
	if got := mr.CommandCount() - before; got != 1 {
		t.Errorf("the upload made %d Redis commands, want the one path lookup", got)
	}
	msgs := cc.stream.(*fakeCoreStream).messages()
	if len(msgs) != 2 || msgs[0].GetResult().GetMessage() != "ready for chunks" || msgs[1].GetResult().GetMessage() != "written" {
		t.Fatalf("replies = %v, want ready for chunks then written", msgs)
	}
	if b, err := os.ReadFile(filepath.Join(base, run18UUID, "survival", "a.txt")); err != nil || string(b) != "abc" {
		t.Fatalf("uploaded file = %q, %v", b, err)
	}
}

// Y01: the storage-key GET had no deadline, so against a Redis that hangs on
// an established connection go-redis retried it for ~12s on every lookup.
func TestTheStorageKeyReadDoesNotWaitOutAHungRedis(t *testing.T) {
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
	rdb := redis.NewClient(&redis.Options{Addr: ln.Addr().String()})
	defer rdb.Close()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, run18UUID), 0o755); err != nil {
		t.Fatal(err)
	}
	sm := NewStorageManager(base, rdb)

	start := time.Now()
	got := sm.GetServerPath(run18UUID)
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("GetServerPath took %v against a hung Redis", took)
	}
	if got != base {
		t.Fatalf("GetServerPath = %q, want the scanned %q", got, base)
	}
}
