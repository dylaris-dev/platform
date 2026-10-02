package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "dylaris-proto/node"
)

// Over Core's message limit the whole stream failed, not the one reply - and
// a container answering a tab request with 200 KB of headers did exactly
// that, for every tenant on the node.
func TestAnOversizedReplyBecomesAnErrorForThatRequest(t *testing.T) {
	big := &pb.NodeMessage{RequestId: "r1", Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 500, Message: strings.Repeat("x", 200<<10)}}}
	out := boundForCore(big)
	if out.GetRequestId() != "r1" || out.GetError() == nil || len(out.GetError().Message) > 1024 {
		t.Fatalf("got %d bytes for %q", len(out.GetError().GetMessage()), out.GetRequestId())
	}
	small := &pb.NodeMessage{RequestId: "r2", Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 500, Message: "ok"}}}
	if boundForCore(small) != small {
		t.Fatal("a reply within the limit was replaced")
	}
}

// Go's client accepts 10 MB of response headers by default.
func TestTheTabProxyClientRefusesHugeHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("a", 200<<10))
	}))
	defer srv.Close()
	if resp, err := proxyHTTPClient.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("200 KB of response headers were accepted")
	}
}

// One server could take every bridge on the node.
func TestOneServerCannotTakeEveryWSBridge(t *testing.T) {
	m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
	cc := &coreConnection{stream: &fakeCoreStream{}}
	for i := 0; i < maxWSBridgesPerServer; i++ {
		m.wsBridges[fmt.Sprintf("pre-%d", i)] = &wsBridge{server: "greedy", done: make(chan struct{})}
	}
	before := len(m.wsBridges)
	m.handleWSOpen(cc, "more", "greedy", &pb.WsOpen{TargetPort: 8080, Path: "/"})
	if len(m.wsBridges) != before {
		t.Fatal("a server opened a bridge past its own cap")
	}
}
