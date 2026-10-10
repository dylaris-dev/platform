package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pb "dylaris-proto/node"
)

func TestCoreStripHopByHop(t *testing.T) {
	in := []*pb.HttpHeader{
		{Key: "Content-Type", Value: "text/html"},
		{Key: "Connection", Value: "keep-alive"},
		{Key: "Transfer-Encoding", Value: "chunked"},
		{Key: "Upgrade", Value: "websocket"},
		{Key: "Set-Cookie", Value: "session=evil; Path=/"},
		{Key: "set-cookie2", Value: "session2=evil"},
		{Key: "Cache-Control", Value: "public, max-age=3600"},
		{Key: "Expires", Value: "Wed, 21 Oct 2099 07:28:00 GMT"},
		{Key: "Pragma", Value: "cache"},
		{Key: "X-Keep", Value: "yes"},
	}
	got := coreStripHopByHop(in)
	kept := map[string]bool{}
	for _, h := range got {
		kept[h.Key] = true
	}
	for _, drop := range []string{"Connection", "Transfer-Encoding", "Upgrade", "Set-Cookie", "set-cookie2", "Cache-Control", "Expires", "Pragma"} {
		if kept[drop] {
			t.Errorf("header %q not stripped", drop)
		}
	}
	for _, keep := range []string{"Content-Type", "X-Keep"} {
		if !kept[keep] {
			t.Errorf("header %q dropped", keep)
		}
	}
}

// TestWriteProxyHeaders_CacheControlStaysSingle covers the WS5/B5
// cache-correctness fix: serveHTTP always stamps its own authoritative
// Cache-Control (browserCacheControl) via Set BEFORE relaying the
// container's response headers via writeProxyHeaders (which uses Add). If a
// container-supplied Cache-Control (or Expires/Pragma) were not stripped
// first, the response would carry two Cache-Control values and a lenient
// shared cache on the panel origin could honor the container's permissive
// one for this per-user/per-ticket content.
func TestWriteProxyHeaders_CacheControlStaysSingle(t *testing.T) {
	rec := httptest.NewRecorder()
	hs := []*pb.HttpHeader{
		{Key: "Content-Type", Value: "text/html"},
		{Key: "Cache-Control", Value: "public, max-age=3600"},
		{Key: "Expires", Value: "Wed, 21 Oct 2099 07:28:00 GMT"},
		{Key: "Pragma", Value: "cache"},
		{Key: "Set-Cookie", Value: "session=evil; Path=/"},
	}
	// Mirrors the real call order in serveHTTP: Core's value is Set first.
	rec.Header().Set("Cache-Control", browserCacheControl(headerValue(hs, "Cache-Control")))
	writeProxyHeaders(rec, hs, false)

	if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "private, max-age=300" {
		t.Errorf("Cache-Control = %v, want exactly [\"private, max-age=300\"]", got)
	}
	if got := rec.Header().Get("Expires"); got != "" {
		t.Errorf("Expires = %q, want stripped", got)
	}
	if got := rec.Header().Get("Pragma"); got != "" {
		t.Errorf("Pragma = %q, want stripped", got)
	}
	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("Set-Cookie = %v, want stripped", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html" {
		t.Errorf("Content-Type = %q, want text/html (unrelated header must pass through)", got)
	}
}

// TestWriteProxyHeaders_ForcesNosniffAndDropsSetCookie covers the WS5
// final-review hardening: writeProxyHeaders is the single chokepoint both
// InDashboard and Public relay a container's response headers through, so
// this exercises the http.ResponseWriter side directly (coreStripHopByHop
// above only covers the pure header-list filtering).
func TestWriteProxyHeaders_ForcesNosniffAndDropsSetCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProxyHeaders(rec, []*pb.HttpHeader{
		{Key: "Content-Type", Value: "text/html"},
		{Key: "Set-Cookie", Value: "session=evil; Path=/"},
		{Key: "X-Content-Type-Options", Value: "sniff-me-anyway"},
	}, false)

	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("Set-Cookie = %v, want stripped", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html" {
		t.Errorf("Content-Type = %q, want text/html (unrelated header must pass through)", got)
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	cases := []struct {
		name       string
		upgrade    string
		connection string
		want       bool
	}{
		{"ws upgrade", "websocket", "Upgrade", true},
		{"ws upgrade mixed case", "WebSocket", "keep-alive, Upgrade", true},
		{"no upgrade header", "", "", false},
		{"upgrade but not ws", "h2c", "Upgrade", false},
		{"ws but connection missing", "websocket", "", false},
	}
	for _, c := range cases {
		r, _ := http.NewRequest("GET", "/x", nil)
		if c.upgrade != "" {
			r.Header.Set("Upgrade", c.upgrade)
		}
		if c.connection != "" {
			r.Header.Set("Connection", c.connection)
		}
		if got := isWebSocketUpgrade(r); got != c.want {
			t.Errorf("%s: isWebSocketUpgrade = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSingleJoin(t *testing.T) {
	cases := []struct {
		base, sub, want string
	}{
		{"/", "", "/"},
		{"/", "tiles/x.png", "/tiles/x.png"},
		{"/map", "tiles", "/map/tiles"},
		{"/map/", "/tiles", "/map/tiles"},
		{"/map", "/tiles", "/map/tiles"},
	}
	for _, c := range cases {
		if got := singleJoin(c.base, c.sub); got != c.want {
			t.Errorf("singleJoin(%q,%q) = %q, want %q", c.base, c.sub, got, c.want)
		}
	}
}

// TestSanitizeProxyPath covers security invariant #2 carried forward from the
// WS5 task reviews: the path Core forwards to the node in HttpProxyReq.Path
// MUST be origin-form starting with exactly one "/", never "//", so a value
// like "//evil.com/x" can't be mistaken for a scheme-relative URL by any
// consumer that re-parses it, and must never carry a raw CR/LF that could
// enable request-line/header injection when the node concatenates it into a
// raw URL string ("http://" + addr + path, see node/grpc_tabproxy.go).
func TestSanitizeProxyPath(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"already clean", "/a/b", "/a/b"},
		{"missing leading slash", "a/b", "/a/b"},
		{"empty", "", "/"},
		{"double leading slash", "//evil.com/x", "/evil.com/x"},
		{"triple leading slash", "///evil.com", "/evil.com"},
		{"scheme-relative attempt with query", "//evil.com/x?y=1", "/evil.com/x?y=1"},
		{"CR LF stripped", "/a\r\nEvil-Header: 1", "/aEvil-Header: 1"},
		{"embedded double slash mid path kept", "/a//b", "/a//b"},
		{"at-sign userinfo-style attempt", "/@evil.com/x", "/@evil.com/x"},
	}
	for _, c := range cases {
		if got := sanitizeProxyPath(c.in); got != c.want {
			t.Errorf("%s: sanitizeProxyPath(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// What the table above actually protects is not a string shape, it is where the
// node's "http://"+addr+path ends up pointing. Assert that directly, built the
// way the node builds it, so a future rewrite of sanitizeProxyPath is measured
// against the outcome instead of against its own expectations.
//
// The case that makes this necessary is "@evil.com/x": net/url reads
// "http://10.0.0.5:8080@evil.com/x" as host evil.com with the container address
// demoted to userinfo, so a path reaching the node without a leading "/" walks
// straight past the node's private-IP guard and turns the tab proxy into an
// SSRF pivot onto the node's network.
func TestSanitizedPathCannotRetargetTheNodesOutboundHost(t *testing.T) {
	const container = "10.0.0.5:8080"
	for _, raw := range []string{
		"@evil.com/x",
		"@evil.com/x?a=1",
		"@evil.com:9/x",
		"//evil.com/x",
		"/@evil.com/x",
		"evil.com",
		"",
	} {
		req, err := http.NewRequest(http.MethodGet, "http://"+container+sanitizeProxyPath(raw), nil)
		if err != nil {
			t.Errorf("%q: the node's URL build failed outright: %v", raw, err)
			continue
		}
		if req.URL.Host != container {
			t.Errorf("%q: the node would dial %q instead of %q, past its private-IP guard",
				raw, req.URL.Host, container)
		}
	}
}
