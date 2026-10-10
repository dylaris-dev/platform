package handlers

import (
	"cmp"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	nodegrpc "dylaris-core/grpc"

	pb "dylaris-proto/node"
)

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   bool
	}{
		{"absent", nil, false},
		{"empty", []string{""}, false},
		{"browser default", []string{"gzip, deflate, br, zstd"}, true},
		{"identity only", []string{"identity"}, false},
		{"br only", []string{"br"}, false},
		{"case and spaces", []string{" GZip ;q=0.8"}, true},
		{"refused q=0", []string{"gzip;q=0, br"}, false},
		{"refused q=0.000", []string{"gzip; q=0.000"}, false},
		{"garbage q", []string{"gzip;q=abc"}, false},
		{"second header line", []string{"br", "gzip"}, true},
		{"x-gzip is not gzip", []string{"x-gzip"}, false},
		{"wildcard is not trusted", []string{"*"}, false},
	}
	for _, c := range cases {
		if got := acceptsGzip(c.values); got != c.want {
			t.Errorf("%s: acceptsGzip(%q) = %v, want %v", c.name, c.values, got, c.want)
		}
	}
}

func TestBrowserCacheControl(t *testing.T) {
	cases := []struct {
		container string
		want      string
	}{
		{"", "no-store"},
		{"max-age=86400", "private, max-age=300"}, // BlueMap tile: capped to the ticket TTL
		{"public, max-age=120", "private, max-age=120"},
		{"max-age=\"120\"", "private, max-age=120"},
		{"max-age=0", "no-store"},
		{"max-age=-5", "no-store"},
		{"max-age=abc", "no-store"},
		{"no-cache", "private, no-cache"}, // BlueMap live/players.json
		{"No-Cache, max-age=600", "private, no-cache"},
		{"no-store, max-age=600", "no-store"},
		{"no-cache, no-store", "no-store"},
		{"private, max-age=600", "no-store"},
		{"public", "no-store"},
		{"must-revalidate", "no-store"},
	}
	for _, c := range cases {
		got := browserCacheControl(c.container)
		if got != c.want {
			t.Errorf("browserCacheControl(%q) = %q, want %q", c.container, got, c.want)
		}
		if strings.Contains(got, "public") {
			t.Errorf("browserCacheControl(%q) = %q, must never be public", c.container, got)
		}
	}
}

// tabNode answers one HttpProxyReq the way node/grpc_tabproxy.go does: a
// head, the body chunk, then the final TransferDone. It records the request
// headers Core forwarded.
type tabNode struct {
	grpc.ServerStream
	conn *nodegrpc.NodeConnection
	head []*pb.HttpHeader
	body []byte
	// status defaults to 200.
	status int32
	mu     sync.Mutex
	got    []*pb.HttpHeader
}

func (s *tabNode) Context() context.Context       { return context.Background() }
func (s *tabNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *tabNode) Send(m *pb.NodeMessage) error {
	req := m.GetHttpProxyReq()
	if req == nil {
		return nil
	}
	s.mu.Lock()
	s.got = req.Headers
	conn := s.conn
	s.mu.Unlock()
	id := m.RequestId
	go func() {
		conn.RouteResponse(&pb.NodeMessage{RequestId: id, Payload: &pb.NodeMessage_HttpProxyRespHead{
			HttpProxyRespHead: &pb.HttpProxyRespHead{StatusCode: cmp.Or(s.status, 200), Headers: s.head}}})
		conn.RouteResponse(&pb.NodeMessage{RequestId: id, Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: s.body}}})
		conn.RouteResponse(&pb.NodeMessage{RequestId: id, Payload: &pb.NodeMessage_TransferDone{
			TransferDone: &pb.TransferDone{TotalBytes: int64(len(s.body))}}})
		// What grpc/server.go does on a TransferDone: it ends the stream.
		conn.CloseStreamingRequest(id)
	}()
	return nil
}

func serveThroughTabNode(t *testing.T, acceptEncoding string, node *tabNode) (*httptest.ResponseRecorder, []*pb.HttpHeader) {
	t.Helper()
	reg := nodegrpc.NewRegistry()
	conn := reg.Register(7, "tok", node)
	node.mu.Lock()
	node.conn = conn
	node.mu.Unlock()
	h := &ProxyHandler{state: &AppState{GRPCRegistry: reg}}
	r := httptest.NewRequest(http.MethodGet, "http://tab.example/maps/world/tiles/1/x0/z0.prbm", nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	h.serveHTTP(w, r, &proxyTab{ID: 1, ServerUUID: "srv-1", NodeID: 7, TargetPort: 8100, TargetPath: "/"}, "maps/world/tiles/1/x0/z0.prbm")
	node.mu.Lock()
	defer node.mu.Unlock()
	return w, node.got
}

func forwardedEncodings(hs []*pb.HttpHeader) []string {
	var out []string
	for _, h := range hs {
		if strings.EqualFold(h.Key, "Accept-Encoding") {
			out = append(out, h.Value)
		}
	}
	return out
}

// A BlueMap tile, as measured: gzip on the wire, chunked, max-age=86400. Core
// must ask for gzip, hand the compressed bytes through untouched with their
// Content-Encoding and Vary, and let the browser cache it privately for at
// most an hour.
func TestTabProxyRelaysAGzipTileCompressed(t *testing.T) {
	gz := []byte("\x1f\x8b\x08\x00compressed-tile")
	node := &tabNode{body: gz, head: []*pb.HttpHeader{
		{Key: "Content-Type", Value: "application/octet-stream"},
		{Key: "Content-Encoding", Value: "gzip"},
		{Key: "Vary", Value: "Accept-Encoding"},
		{Key: "Cache-Control", Value: "max-age=86400"},
	}}
	w, fwd := serveThroughTabNode(t, "gzip, deflate, br, zstd", node)

	if got := forwardedEncodings(fwd); len(got) != 1 || got[0] != "gzip" {
		t.Fatalf("forwarded Accept-Encoding = %v, want exactly [gzip]", got)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	if got := w.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want none (the container sent none)", got)
	}
	if got := w.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "private, max-age=300" {
		t.Errorf("Cache-Control = %v, want [private, max-age=300]", got)
	}
	if w.Body.String() != string(gz) {
		t.Errorf("body = %q, want the compressed bytes unchanged", w.Body.String())
	}
}

// A browser that does not take gzip gets no Accept-Encoding forwarded, so the
// node's transport keeps its own transparent gzip as before; and a container
// that says nothing about caching stays no-store.
func TestTabProxyForwardsNoEncodingTheBrowserRefused(t *testing.T) {
	for _, ae := range []string{"", "identity", "gzip;q=0, br"} {
		node := &tabNode{body: []byte("{}"), head: []*pb.HttpHeader{{Key: "Content-Type", Value: "application/json"}}}
		w, fwd := serveThroughTabNode(t, ae, node)
		if got := forwardedEncodings(fwd); len(got) != 0 {
			t.Errorf("Accept-Encoding %q: forwarded %v, want none", ae, got)
		}
		if got := w.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
			t.Errorf("Accept-Encoding %q: Cache-Control = %v, want [no-store]", ae, got)
		}
	}
}

// The WebSocket open shares forwardRequestHeaders and must not pick up the
// gzip that only serveHTTP adds.
func TestForwardRequestHeadersKeepsNoAcceptEncoding(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip, br")
	if got := forwardedEncodings((&ProxyHandler{}).forwardRequestHeaders(r)); len(got) != 0 {
		t.Errorf("forwardRequestHeaders forwarded Accept-Encoding %v, want none", got)
	}
}

// A 304 that carries no Cache-Control must not get no-store stamped on it:
// the browser would apply that to the entry it just revalidated and drop it.
func TestTabProxyLeaves304WithoutCacheControlAlone(t *testing.T) {
	node := &tabNode{status: http.StatusNotModified, head: []*pb.HttpHeader{{Key: "ETag", Value: `"t1"`}}}
	w, _ := serveThroughTabNode(t, "gzip", node)
	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w.Code)
	}
	if got := w.Header().Values("Cache-Control"); len(got) != 0 {
		t.Errorf("Cache-Control = %v, want none on a bare 304", got)
	}
}
