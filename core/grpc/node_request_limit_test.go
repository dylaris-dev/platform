package nodegrpc

import (
	"context"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// M4: a node may start requests at nodeRequestRate with a burst of
// nodeRequestBurst, however it spreads them over streams and reconnects.
func TestAllowNodeRequestIsATokenBucketPerNode(t *testing.T) {
	reg := NewRegistry()
	start := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	for i := 0; i < int(nodeRequestBurst); i++ {
		if !reg.allowNodeRequest(42, start) {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	if reg.allowNodeRequest(42, start) {
		t.Fatal("a request past the burst was allowed")
	}
	if !reg.allowNodeRequest(7, start) {
		t.Fatal("another node was limited by node 42's requests")
	}
	// One tenth of a second refills one request at 10 per second, and no more.
	later := start.Add(100 * time.Millisecond)
	if !reg.allowNodeRequest(42, later) {
		t.Fatal("the bucket did not refill")
	}
	if reg.allowNodeRequest(42, later) {
		t.Fatal("the bucket refilled more than the rate allows")
	}
	// A long silence refills to the burst, never beyond it.
	idle := later.Add(time.Hour)
	for i := 0; i < int(nodeRequestBurst); i++ {
		if !reg.allowNodeRequest(42, idle) {
			t.Fatalf("after an hour idle, request %d was refused", i+1)
		}
	}
	if reg.allowNodeRequest(42, idle) {
		t.Fatal("an idle bucket filled beyond the burst")
	}
}

// M4: an exhausted node is answered NodeRequestBusy without its handler running,
// and dropping the stream and connecting again does not refill the bucket.
func TestServeNodeRequestLimitSurvivesAReconnect(t *testing.T) {
	reg := NewRegistry()
	served := make(chan struct{}, 1)
	reg.HandleNodeRequest("complete_upload_request", func(context.Context, Node, *pb.NodeMessage) *pb.NodeMessage {
		served <- struct{}{}
		return &pb.NodeMessage{}
	})
	node := Node{ID: 42, Token: "node-abc"}
	req := func(id string) *pb.NodeMessage {
		return &pb.NodeMessage{RequestId: id, NodeRequest: true,
			Payload: &pb.NodeMessage_CompleteUploadRequest{CompleteUploadRequest: &pb.CompleteUploadRequest{}}}
	}

	first := &chanNodeStream{ctx: context.Background()}
	conn := reg.Register(node.ID, node.Token, first)
	now := time.Now()
	for i := 0; i < int(nodeRequestBurst); i++ {
		reg.allowNodeRequest(node.ID, now)
	}
	reg.Unregister(node.ID, conn)

	second := &chanNodeStream{ctx: context.Background()}
	conn = reg.Register(node.ID, node.Token, second)
	reg.serveNodeRequest(context.Background(), node, conn, make(chan struct{}, maxConcurrentNodeRequests), req("after-reconnect"))

	if e := second.waitSent(t, "after-reconnect").GetError(); e == nil || e.Code != NodeRequestBusy {
		t.Fatalf("request after a reconnect: %v, want OpError %d", e, NodeRequestBusy)
	}
	select {
	case <-served:
		t.Fatal("the handler ran for a request over the limit")
	default:
	}
}
