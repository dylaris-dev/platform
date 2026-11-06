package tabs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestWSReassemblyExceeded covers the DoS-mirror fix: the container->browser
// reassembly loop in serveWS must stop growing rxBuf and tear the bridge down
// once the next fragment would push it past maxWSMessageBytes, mirroring the
// node's own wsBridge.appendInbound cap (node/grpc_tabproxy_ws.go). Table-
// tested as a pure function since standing up a full WS/gRPC bridge just to
// exercise the boundary would be heavy scaffolding for no extra coverage.
func TestWSReassemblyExceeded(t *testing.T) {
	cases := []struct {
		name     string
		cur, add int
		max      int
		want     bool
	}{
		{"well under cap", 0, 100, 1000, false},
		{"exactly at cap", 900, 100, 1000, false},
		{"one byte over cap", 900, 101, 1000, true},
		{"already at cap, empty fragment fits", 1000, 0, 1000, false},
		{"already at cap, one more byte overflows", 1000, 1, 1000, true},
		{"realistic 8MiB cap under", 8*1024*1024 - 1, 1, 8 * 1024 * 1024, false},
		{"realistic 8MiB cap over", 8 * 1024 * 1024, 1, 8 * 1024 * 1024, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wsReassemblyExceeded(c.cur, c.add, c.max); got != c.want {
				t.Errorf("wsReassemblyExceeded(cur=%d,add=%d,max=%d) = %v, want %v", c.cur, c.add, c.max, got, c.want)
			}
		})
	}
}

func TestSplitProxyBytes(t *testing.T) {
	cases := []struct {
		name  string
		size  int
		max   int
		wantN int
	}{
		{"empty", 0, 60, 1},
		{"under", 30, 60, 1},
		{"exact", 60, 60, 1},
		{"over", 61, 60, 2},
		{"triple", 130, 60, 3},
		{"zero max falls back", 5, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := make([]byte, c.size)
			for i := range data {
				data[i] = byte(i)
			}
			frags := splitProxyBytes(data, c.max)
			if len(frags) != c.wantN {
				t.Fatalf("splitProxyBytes(size=%d,max=%d) => %d frags, want %d", c.size, c.max, len(frags), c.wantN)
			}
			var joined []byte
			for _, f := range frags {
				joined = append(joined, f...)
			}
			if len(joined) != c.size {
				t.Fatalf("reassembled len = %d, want %d", len(joined), c.size)
			}
			for i := range joined {
				if joined[i] != data[i] {
					t.Fatalf("reassembled[%d] = %d, want %d", i, joined[i], data[i])
				}
			}
		})
	}
}

// The 101 named no subprotocol, so a browser that offered one (ttyd's "tty",
// noVNC's "binary") failed the handshake and the tab's live connection never
// opened.
func TestTheTabWSUpgradeAnswersTheOfferedSubprotocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, wsUpgradeHeader(r))
		if err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	cases := []struct {
		name    string
		offered []string
		want    string
	}{
		{"none offered", nil, ""},
		{"one offered", []string{"tty"}, "tty"},
		{"several offered, the first wins", []string{"binary", "base64"}, "binary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := websocket.Dialer{Subprotocols: c.offered}
			conn, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if got := conn.Subprotocol(); got != c.want {
				t.Fatalf("subprotocol = %q, want %q", got, c.want)
			}
		})
	}
}
