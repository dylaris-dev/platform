package main

import (
	"io"
	"net"
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

// Y02: the transport kept the default ProxyFromEnvironment, so on a node with
// HTTP(S)_PROXY set every tab request to the container's bridge address went
// to that proxy instead, which cannot reach it.
func TestTheTabProxyDialsTheContainerDirectly(t *testing.T) {
	tr, ok := proxyHTTPClient.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("the tab proxy transport routes container requests through the environment's proxy")
	}
}

// B01: ResponseHeaderTimeout only starts once the body is written, so a
// container that never reads a request body held the node's slot forever.
func TestTheTabProxyBoundsTheBodyWriteBeforeTheHead(t *testing.T) {
	prev := proxyHeadTimeout
	t.Cleanup(func() { proxyHeadTimeout = prev })
	proxyHeadTimeout = 300 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept, never read
		}
	}()

	body := io.LimitReader(zeroReader{}, 256<<20)
	hreq, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/", body)
	done := make(chan error, 1)
	go func() {
		resp, err := doProxyRequest(hreq)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request whose body was never read succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a body write the container never reads is not bounded")
	}
}
