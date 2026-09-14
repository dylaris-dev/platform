package nodegrpc

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	pb "dylaris-proto/node"
)

// chanNodeStream is a NodeConnect stream the test feeds while NodeConnect runs,
// which fakeNodeStream cannot do: it hands over a fixed script and records sends
// without a lock.
type chanNodeStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv chan *pb.NodeMessage

	mu   sync.Mutex
	sent []*pb.NodeMessage
}

func (s *chanNodeStream) Context() context.Context { return s.ctx }

func (s *chanNodeStream) Recv() (*pb.NodeMessage, error) {
	m, ok := <-s.recv
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func (s *chanNodeStream) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

// waitSent returns the first sent message with this request_id.
func (s *chanNodeStream) waitSent(t *testing.T, requestID string) *pb.NodeMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, m := range s.sent {
			if m.RequestId == requestID {
				s.mu.Unlock()
				return m
			}
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing was sent with request_id %q", requestID)
	return nil
}

// A node-initiated request goes to its handler with the identity the STREAM
// authenticated, on a goroutine of its own, and its answer comes back under the
// same request_id. The payload deliberately names a different node in every
// field that could be mistaken for one.
func TestNodeInitiatedRequestIsServedOffTheReadLoopAsTheStreamsNode(t *testing.T) {
	reg := NewRegistry()

	type call struct {
		node Node
		msg  *pb.NodeMessage
	}
	calls := make(chan call, 1)
	release := make(chan struct{})
	reg.HandleNodeRequest("upload_part_urls_request", func(_ context.Context, node Node, msg *pb.NodeMessage) *pb.NodeMessage {
		calls <- call{node, msg}
		<-release
		return &pb.NodeMessage{
			RequestId: "the handler may not choose this",
			Payload: &pb.NodeMessage_UploadPartUrlsResponse{UploadPartUrlsResponse: &pb.UploadPartUrlsResponse{
				UploadId: "up-1", PartSize: 64 << 20,
			}},
		}
	})

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234},
	})
	stream := &chanNodeStream{ctx: ctx, recv: make(chan *pb.NodeMessage, 8)}
	stream.recv <- authMsg(authFor("node-abc"))
	stream.recv <- &pb.NodeMessage{Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: &pb.NodeChallengeResponse{Response: "ok"}}}

	srv := NewServer(reg, knownNodeLookup{token: "node-abc"}, "core-test", provisionedACL{verdict: true}, nil, nil)
	done := make(chan error, 1)
	go func() { done <- srv.NodeConnect(stream) }()

	deadline := time.Now().Add(2 * time.Second)
	for !reg.IsConnected(42) {
		if time.Now().After(deadline) {
			t.Fatal("the node never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Core asks the node something of its own first, so a reply is pending when
	// the node's request arrives.
	coreResp := make(chan error, 1)
	go func() {
		_, err := reg.SendRequest(42, &pb.NodeMessage{RequestId: "core-1"}, 2*time.Second)
		coreResp <- err
	}()
	stream.waitSent(t, "core-1")

	stream.recv <- &pb.NodeMessage{
		RequestId:   "node-1",
		ServerUuid:  "node-evil",
		NodeRequest: true,
		Payload: &pb.NodeMessage_UploadPartUrlsRequest{UploadPartUrlsRequest: &pb.UploadPartUrlsRequest{
			RunId: "node-evil", PartNumbers: []int32{1, 2},
		}},
	}
	var got call
	select {
	case got = <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler was never called")
	}
	if got.node.ID != 42 || got.node.Token != "node-abc" {
		t.Errorf("handler got node %+v, want the stream's node (ID 42, token node-abc)", got.node)
	}

	// The handler is still blocked. The node's reply to core-1 has to get
	// through anyway.
	stream.recv <- &pb.NodeMessage{RequestId: "core-1", Payload: &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "ok"}}}
	select {
	case err := <-coreResp:
		if err != nil {
			t.Fatalf("Core's own request failed while a node request was being handled: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Core's own request was not answered while a node request was being handled: the handler runs on the read loop")
	}

	close(release)
	reply := stream.waitSent(t, "node-1")
	if !reply.NodeRequest {
		t.Error("the reply does not carry the node_request flag")
	}
	if r := reply.GetUploadPartUrlsResponse(); r == nil || r.UploadId != "up-1" {
		t.Errorf("reply payload = %v, want the handler's UploadPartUrlsResponse", reply.Payload)
	}

	// No handler for this kind, and a kind this Core's proto does not know,
	// which decodes to no payload: both are refused out loud, not dropped.
	stream.recv <- &pb.NodeMessage{
		RequestId: "node-2", NodeRequest: true,
		Payload: &pb.NodeMessage_RestoreUrlRequest{RestoreUrlRequest: &pb.RestoreUrlRequest{RestoreId: "r-1"}},
	}
	stream.recv <- &pb.NodeMessage{RequestId: "node-3", NodeRequest: true}
	for _, id := range []string{"node-2", "node-3"} {
		r := stream.waitSent(t, id)
		if e := r.GetError(); e == nil || e.Code != NodeRequestUnsupported || !r.NodeRequest {
			t.Errorf("%s: reply = %v (node_request=%v), want OpError %d with the flag", id, r.Payload, r.NodeRequest, NodeRequestUnsupported)
		}
	}

	close(stream.recv)
	if err := <-done; err != nil {
		t.Fatalf("NodeConnect: %v", err)
	}
}

// A handler that panics answers the node instead of taking Core down, and one
// stream cannot hold more handler goroutines than the bound.
func TestNodeRequestHandlerPanicAndBusy(t *testing.T) {
	reg := NewRegistry()
	block := make(chan struct{})
	defer close(block)
	reg.HandleNodeRequest("complete_upload_request", func(context.Context, Node, *pb.NodeMessage) *pb.NodeMessage {
		<-block
		return &pb.NodeMessage{}
	})
	reg.HandleNodeRequest("restore_url_request", func(context.Context, Node, *pb.NodeMessage) *pb.NodeMessage {
		panic("boom")
	})
	stream := &chanNodeStream{ctx: context.Background()}
	conn := reg.Register(42, "node-abc", stream)
	node := Node{ID: 42, Token: "node-abc"}

	slots := make(chan struct{}, 1)
	req := func(id string, p any) *pb.NodeMessage {
		m := &pb.NodeMessage{RequestId: id, NodeRequest: true}
		switch v := p.(type) {
		case *pb.CompleteUploadRequest:
			m.Payload = &pb.NodeMessage_CompleteUploadRequest{CompleteUploadRequest: v}
		case *pb.RestoreUrlRequest:
			m.Payload = &pb.NodeMessage_RestoreUrlRequest{RestoreUrlRequest: v}
		}
		return m
	}
	returned := make(chan struct{})
	go func() {
		reg.serveNodeRequest(context.Background(), node, conn, slots, req("held", &pb.CompleteUploadRequest{}))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("serveNodeRequest waited for its handler instead of returning to the read loop")
	}
	reg.serveNodeRequest(context.Background(), node, conn, slots, req("busy", &pb.CompleteUploadRequest{}))
	if e := stream.waitSent(t, "busy").GetError(); e == nil || e.Code != NodeRequestBusy {
		t.Errorf("second request past the bound: %v, want OpError %d", e, NodeRequestBusy)
	}

	reg.serveNodeRequest(context.Background(), node, conn, make(chan struct{}, 1), req("panics", &pb.RestoreUrlRequest{}))
	if e := stream.waitSent(t, "panics").GetError(); e == nil || e.Code != NodeRequestHandlerFailed {
		t.Errorf("panicking handler: %v, want OpError %d", e, NodeRequestHandlerFailed)
	}
}

func TestHandleNodeRequestRejectsAnUnknownKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering a kind that is not a payload field did not panic")
		}
	}()
	NewRegistry().HandleNodeRequest("upload_parts_urls_request", nil)
}
