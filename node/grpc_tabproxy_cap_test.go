package main

import (
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// L12: every HttpProxyReq ran on its own goroutine with its own container
// connection and nothing bounded how many, and a container that trickles a
// response keeps each one open. One tenant could exhaust the shared node agent.
// Past the cap a request is answered busy at once, per server and per node.
func TestTabProxyRequestsAreCappedPerServerAndPerNode(t *testing.T) {
	const full = "11111111-1111-4111-8111-111111111111"
	const other = "22222222-2222-4222-8222-222222222222"
	prevNode := proxyNodeSlots
	t.Cleanup(func() { proxyNodeSlots = prevNode })

	cases := []struct {
		name     string
		nodeCap  int
		fillNode int
		target   string
		wantBusy bool
	}{
		{"a server at its cap", 1024, 0, full, true},
		{"another server is not held back by it", 1024, 0, other, false},
		{"the node at its cap refuses every server", 2, 2, other, true},
	}
	serverSlots := proxySlots.of(full, proxiesPerServer)
	for i := 0; i < proxiesPerServer; i++ {
		serverSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(serverSlots) > 0 {
			<-serverSlots
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyNodeSlots = make(chan struct{}, tc.nodeCap)
			for i := 0; i < tc.fillNode; i++ {
				proxyNodeSlots <- struct{}{}
			}
			stream := &fakeCoreStream{}
			m := &MeshManager{handler: &StreamHandler{}}
			// Port 0 is answered "invalid proxy target" without dialling, so a
			// request that got a slot is told apart from one refused for it.
			m.handleRequest(&coreConnection{stream: stream}, &pb.NodeMessage{RequestId: "p", ServerUuid: tc.target, Payload: &pb.NodeMessage_HttpProxyReq{HttpProxyReq: &pb.HttpProxyReq{}}})
			deadline := time.Now().Add(2 * time.Second)
			for len(stream.messages()) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			msgs := stream.messages()
			if len(msgs) != 1 {
				t.Fatalf("got %v, want one answer", msgs)
			}
			busy := msgs[0].GetError().GetCode() == 503
			if busy != tc.wantBusy {
				t.Fatalf("got %v, want busy=%v", msgs[0], tc.wantBusy)
			}
			if !tc.wantBusy {
				// The slot is given back once the request ends.
				deadline := time.Now().Add(2 * time.Second)
				for len(proxyNodeSlots) != tc.fillNode && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if n := len(proxyNodeSlots); n != tc.fillNode {
					t.Fatalf("%d node slot(s) still held after the request ended", n-tc.fillNode)
				}
			}
		})
	}
}
