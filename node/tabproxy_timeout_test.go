package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// Client.Timeout also covers reading the body, so every tab response still
// streaming 30 s after the request began was cut: SSE, long polls, a large or
// throttled download. Only the head is bounded in total.
func TestTheTabProxyBoundsTheHeadNotTheBody(t *testing.T) {
	if proxyHTTPClient.Timeout != 0 {
		t.Fatalf("Client.Timeout = %v caps the whole body", proxyHTTPClient.Timeout)
	}
	tr, ok := proxyHTTPClient.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("no response header timeout: a container that never answers holds the request forever")
	}
}

// A body that keeps moving outlives the idle bound, and one that stops is
// ended by it rather than holding the node's goroutine and connection forever.
func TestTheTabProxyIdleBoundIsPerRead(t *testing.T) {
	prev := proxyIdleTimeout
	t.Cleanup(func() { proxyIdleTimeout = prev })
	proxyIdleTimeout = 300 * time.Millisecond

	cases := []struct {
		name     string
		hang     bool
		wantDone bool
	}{
		{"a drip longer than the bound in total", false, true},
		{"a body that stops sending", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i < 6; i++ {
					w.Write([]byte("data: x\n\n"))
					w.(http.Flusher).Flush()
					time.Sleep(100 * time.Millisecond)
				}
				if c.hang {
					<-r.Context().Done()
				}
			}))
			defer srv.Close()
			hreq, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			resp, err := doProxyRequest(hreq)
			if err != nil {
				t.Fatal(err)
			}
			var sent []*pb.NodeMessage
			finished := make(chan struct{})
			go func() {
				(&StreamHandler{}).streamProxyResponse("r", resp, func(m *pb.NodeMessage) error { sent = append(sent, m); return nil })
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("an idle body was never ended")
			}
			last := sent[len(sent)-1]
			if got := last.GetTransferDone() != nil; got != c.wantDone {
				t.Fatalf("ended with %v, want done=%v", last, c.wantDone)
			}
		})
	}
}
