package main

import (
	"testing"

	pb "dylaris-proto/node"
)

func TestSplitWSFragments(t *testing.T) {
	cases := []struct {
		name  string
		size  int
		max   int
		wantN int
	}{
		{"empty", 0, 60, 1},
		{"under max", 10, 60, 1},
		{"exact max", 60, 60, 1},
		{"just over", 61, 60, 2},
		{"three fragments", 130, 60, 3},
		{"zero max falls back", 10, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := make([]byte, c.size)
			for i := range data {
				data[i] = byte(i)
			}
			frags := splitWSFragments(data, c.max)
			if len(frags) != c.wantN {
				t.Fatalf("splitWSFragments(size=%d,max=%d) => %d frags, want %d", c.size, c.max, len(frags), c.wantN)
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
			effMax := c.max
			if effMax <= 0 {
				effMax = wsFragmentSize
			}
			for i, f := range frags {
				if len(f) > effMax {
					t.Errorf("fragment %d len %d exceeds max %d", i, len(f), effMax)
				}
			}
		})
	}
}

// newTestBridge builds a wsBridge with a nil *websocket.Conn: wsBridge.close()
// only touches conn when it is non-nil, so the teardown/overflow logic below
// is fully exercisable without dialing a real container WS or a live mesh.
func newTestBridge(owner *coreConnection, cap int) *wsBridge {
	return &wsBridge{
		owner:   owner,
		done:    make(chan struct{}),
		inbound: make(chan *pb.NodeMessage, cap),
	}
}

func isClosed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// TestRouteWSInboundBuffering covers WS5 I2-inbound: the enqueue into
// bridge.inbound must never block the shared per-connection read loop. When
// there is room the message is queued and the bridge stays open; when the
// buffer is already full the bridge is dropped (closed + unregistered)
// instead of blocking, and the overflowing message is not appended.
func TestRouteWSInboundBuffering(t *testing.T) {
	cases := []struct {
		name         string
		cap          int
		preFill      int
		wantEnqueued bool
	}{
		{"room available on empty buffer", 2, 0, true},
		{"room for exactly the last slot", 2, 1, true},
		{"buffer already full", 2, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
			cc := &coreConnection{stream: &fakeCoreStream{}}
			bridge := newTestBridge(cc, c.cap)
			m.wsBridges["r1"] = bridge
			for i := 0; i < c.preFill; i++ {
				bridge.inbound <- &pb.NodeMessage{RequestId: "r1"}
			}

			m.routeWSInbound("r1", &pb.NodeMessage{RequestId: "r1"})

			_, stillRegistered := m.wsBridges["r1"]
			if c.wantEnqueued {
				if wantLen := c.preFill + 1; len(bridge.inbound) != wantLen {
					t.Fatalf("inbound len = %d, want %d", len(bridge.inbound), wantLen)
				}
				if !stillRegistered {
					t.Fatalf("bridge should remain registered when the enqueue succeeded")
				}
				if isClosed(bridge.done) {
					t.Fatalf("bridge.done closed unexpectedly on a successful enqueue")
				}
			} else {
				if len(bridge.inbound) != c.preFill {
					t.Fatalf("inbound len = %d, want unchanged %d (overflow must not block or append)", len(bridge.inbound), c.preFill)
				}
				if stillRegistered {
					t.Fatalf("overflowed bridge should have been removed from wsBridges")
				}
				if !isClosed(bridge.done) {
					t.Fatalf("overflowed bridge.done should be closed")
				}
			}
		})
	}
}

// TestRouteWSInboundUnknownRequestIsNoop guards the existing "no bridge for
// this request_id" branch: it must return without touching any state, and
// must not panic (e.g. after a bridge already closed and unregistered).
func TestRouteWSInboundUnknownRequestIsNoop(t *testing.T) {
	m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
	m.routeWSInbound("missing", &pb.NodeMessage{RequestId: "missing"})
	if len(m.wsBridges) != 0 {
		t.Fatalf("wsBridges should stay empty, got %d entries", len(m.wsBridges))
	}
}

// TestCloseWSBridgesForConnOnlyClosesOwned covers WS5 I1: when a Core
// connection dies, only the bridges it owns are torn down; bridges owned by
// a different, still-live connection must survive untouched.
func TestCloseWSBridgesForConnOnlyClosesOwned(t *testing.T) {
	m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
	dying := &coreConnection{}
	alive := &coreConnection{}
	b1 := newTestBridge(dying, 1)
	b2 := newTestBridge(dying, 1)
	b3 := newTestBridge(alive, 1)
	m.wsBridges["dying-a"] = b1
	m.wsBridges["dying-b"] = b2
	m.wsBridges["alive-a"] = b3

	m.closeWSBridgesForConn(dying)

	for reqID, b := range map[string]*wsBridge{"dying-a": b1, "dying-b": b2} {
		if !isClosed(b.done) {
			t.Errorf("%s: bridge owned by the dying connection should be closed", reqID)
		}
		if _, ok := m.wsBridges[reqID]; ok {
			t.Errorf("%s: bridge owned by the dying connection should be unregistered", reqID)
		}
	}
	if isClosed(b3.done) {
		t.Fatalf("alive-a: bridge owned by a different connection must not be closed")
	}
	if _, ok := m.wsBridges["alive-a"]; !ok {
		t.Fatalf("alive-a: bridge owned by a different connection must remain registered")
	}
}

// TestCloseWSBridgesForConnIdempotentAfterSelfClose covers the double-close
// race the brief calls out: a bridge's own pump (via closeWSBridge, e.g. on
// a routed WsClose or a ReadMessage error) can tear it down at the same
// moment its owning connection dies. Simulated sequentially here since both
// paths share the same atomic lookup-delete-under-wsMu map discipline: once
// closeWSBridge has removed the entry, closeWSBridgesForConn must find
// nothing to close for it (no double close, no panic).
func TestCloseWSBridgesForConnIdempotentAfterSelfClose(t *testing.T) {
	m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
	cc := &coreConnection{}
	bridge := newTestBridge(cc, 1)
	m.wsBridges["r1"] = bridge

	m.closeWSBridge("r1") // bridge's own pump tears it down first
	if !isClosed(bridge.done) {
		t.Fatalf("closeWSBridge should have closed the bridge")
	}

	m.closeWSBridgesForConn(cc) // owning connection dies right after; must be a no-op

	if len(m.wsBridges) != 0 {
		t.Fatalf("wsBridges should be empty, got %d entries", len(m.wsBridges))
	}
}

// TestCloseAllWSBridgesClosesEveryOwner covers the closeAll (mesh shutdown)
// path: every bridge is closed and unregistered regardless of which
// connection owns it.
func TestCloseAllWSBridgesClosesEveryOwner(t *testing.T) {
	m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
	cc1 := &coreConnection{}
	cc2 := &coreConnection{}
	b1 := newTestBridge(cc1, 1)
	b2 := newTestBridge(cc2, 1)
	m.wsBridges["a"] = b1
	m.wsBridges["b"] = b2

	m.closeAllWSBridges()

	if len(m.wsBridges) != 0 {
		t.Fatalf("wsBridges should be empty after closeAllWSBridges, got %d entries", len(m.wsBridges))
	}
	for name, b := range map[string]*wsBridge{"a": b1, "b": b2} {
		if !isClosed(b.done) {
			t.Errorf("%s: bridge should be closed", name)
		}
	}
}
