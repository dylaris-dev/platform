package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	pb "dylaris-proto/node"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// proto3 refuses to marshal a string that is not UTF-8, and grpc-go ends the
// node's whole control stream on that error: a tenant's file name or a tab's
// response header took the node offline for every tenant on it.
func TestRepliesWithInvalidUTF8StillMarshal(t *testing.T) {
	cases := []struct {
		name string
		msg  *pb.NodeMessage
		got  func(*pb.NodeMessage) string
	}{
		{"file name in a listing", &pb.NodeMessage{RequestId: "r1", Payload: &pb.NodeMessage_ListResp{ListResp: &pb.ListFilesResp{Files: []*pb.FileInfo{{Name: "ok"}, {Name: "a\xffb"}}}}},
			func(m *pb.NodeMessage) string { return m.GetListResp().GetFiles()[1].GetName() }},
		{"tab response header", &pb.NodeMessage{RequestId: "r2", Payload: &pb.NodeMessage_HttpProxyRespHead{HttpProxyRespHead: &pb.HttpProxyRespHead{StatusCode: 200, Headers: []*pb.HttpHeader{{Key: "X-A", Value: "v\x81z"}}}}},
			func(m *pb.NodeMessage) string { return m.GetHttpProxyRespHead().GetHeaders()[0].GetValue() }},
		{"error text", errorMsg("r3", 500, "copy \xfe failed"),
			func(m *pb.NodeMessage) string { return m.GetError().GetMessage() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := boundForCore(c.msg)
			if _, err := proto.Marshal(out); err != nil {
				t.Fatalf("still does not marshal: %v", err)
			}
			if out.GetRequestId() != c.msg.GetRequestId() || out.GetError() != nil && c.msg.GetError() == nil {
				t.Fatalf("the reply was replaced: %v", out)
			}
			if s := c.got(out); s == "" {
				t.Fatal("the field was emptied instead of repaired")
			}
		})
	}
}

// A tab body cut off mid-stream ended with TransferDone, which Core reads as
// the whole page: the browser got a cleanly terminated, truncated response.
func TestATabBodyCutShortIsAnErrorNotADone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var sent []*pb.NodeMessage
	(&StreamHandler{}).streamProxyResponse("r", resp, func(m *pb.NodeMessage) error { sent = append(sent, m); return nil })
	last := sent[len(sent)-1]
	if last.GetTransferDone() != nil || last.GetError() == nil {
		t.Fatalf("a cut-off body ended with %v", last)
	}
}

// A pending bridge dropped for a full buffer told Core nothing, so the
// browser's socket stayed open and dead until the tab was closed.
func TestAPendingBridgeDroppedForAFullBufferTellsCore(t *testing.T) {
	fake := &fakeCoreStream{}
	cc := &coreConnection{stream: fake}
	m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
	b := &wsBridge{owner: cc, server: "s", inbound: make(chan *pb.NodeMessage, 1), done: make(chan struct{})}
	m.wsBridges["r"] = b
	frame := &pb.NodeMessage{RequestId: "r", Payload: &pb.NodeMessage_WsFrame{WsFrame: &pb.WsFrame{Data: []byte("x"), Fin: true}}}
	m.routeWSInbound("r", frame)
	m.routeWSInbound("r", frame) // buffer full while the dial is pending
	if _, ok := m.wsBridges["r"]; ok {
		t.Fatal("bridge kept")
	}
	msgs := fake.messages()
	if len(msgs) != 1 || msgs[0].GetWsClose() == nil {
		t.Fatalf("Core got %v, want one WsClose", msgs)
	}
}

// A staging file that could not be created was answered "ready for chunks";
// every chunk was then dropped and Core waited out its confirm timeout.
func TestAFailedUploadStagingFileIsRefusedUpFront(t *testing.T) {
	cases := []struct {
		err  error
		code int32
	}{
		{syscall.EDQUOT, 413},
		{&fs.PathError{Op: "open", Path: "x", Err: syscall.EDQUOT}, 413},
		{errors.New("read-only file system"), 500},
	}
	for _, c := range cases {
		if got := uploadTempRefusal("r", c.err).GetError().GetCode(); got != c.code {
			t.Errorf("%v: code %d, want %d", c.err, got, c.code)
		}
	}
}

type failGet struct{}

func (failGet) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failGet) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" {
			return errors.New("i/o timeout")
		}
		return next(ctx, cmd)
	}
}
func (failGet) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A GET that failed for a reason other than "no such key" counted the Core as
// gone and cut every transfer and bridge on a live connection.
func TestATransientHeartbeatReadKeepsTheConnection(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.Set("dylaris:core:c1", `{"id":"c1","grpc_addr":"127.0.0.1:1"}`)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdb.AddHook(failGet{})
	conn, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	m := NewMeshManager("", rdb, nil)
	m.connections["c1"] = &coreConnection{conn: conn, cancel: func() {}}
	m.scanCores(context.Background())
	if _, ok := m.connections["c1"]; !ok {
		t.Fatal("a live Core was disconnected on a failed heartbeat read")
	}
}
