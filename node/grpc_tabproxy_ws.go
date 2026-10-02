package main

// Node-side WebSocket bridge for custom-tab proxying (WS5). Core sends WsOpen;
// we dial the container's WS endpoint (same address order as HTTP) and pump
// frames both ways over the mesh until either side closes. Core->node frames
// arrive as separate NodeMessages routed here by request_id; container->Core
// frames are streamed from a background goroutine. Application messages are
// fragmented at wsFragmentSize so a single WsFrame never exceeds Core's
// gRPC MaxRecvMsgSize (128KB).

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	pb "dylaris-proto/node"

	"github.com/gorilla/websocket"
)

// wsFragmentSize caps one WsFrame's data so it fits under Core's 128KB gRPC
// receive limit with envelope overhead.
const wsFragmentSize = 60 * 1024

// maxWSMessageBytes caps one reassembled Core->container application message.
// Without it, a client streaming endless Fin=false fragments (or one huge
// message) grows the bridge's rxBuf until the node OOMs, which takes the mesh
// down for EVERY co-located tenant. Over the cap we drop that one bridge with a
// policy WsClose instead of buffering. The Core side MUST mirror this value.
const maxWSMessageBytes = 8 * 1024 * 1024 // 8 MiB

// maxWSBridges bounds the number of concurrent WS bridges on this node so a
// flood of WsOpens cannot exhaust node memory / goroutines. A new WsOpen past
// the cap is rejected with a policy WsClose.
const maxWSBridges = 256

// maxWSBridgesPerServer keeps one server from taking every bridge on the node.
const maxWSBridgesPerServer = 16

// wsBridge tracks one open container WS for a request_id.
type wsBridge struct {
	conn      *websocket.Conn
	server    string               // the server UUID it belongs to, for the per-server cap
	owner     *coreConnection      // Core connection that opened this bridge (WS5 I1 reaper)
	inbound   chan *pb.NodeMessage // Core->node WsFrame/WsClose for this request_id
	done      chan struct{}
	closeOnce sync.Once
	// reassembly buffer for Core->node fragmented application messages
	rxBuf    []byte
	rxOpcode int
}

func (b *wsBridge) close() {
	b.closeOnce.Do(func() {
		close(b.done)
		if b.conn != nil {
			b.conn.Close()
		}
	})
}

// appendInbound accumulates one Core->container WsFrame into the reassembly
// buffer. Called only from the single Core->container pump goroutine, so it
// needs no locking. It returns overflow=true when appending fr.Data would push
// the reassembled message past maxWSMessageBytes (the caller must then drop the
// bridge instead of buffering); rxBuf is left unchanged in that case. When the
// frame carries Fin it returns flush=true with the fully reassembled payload +
// opcode to write to the container and resets the buffer; otherwise flush=false
// and the caller waits for more fragments.
func (b *wsBridge) appendInbound(fr *pb.WsFrame) (payload []byte, opcode int, flush, overflow bool) {
	if len(b.rxBuf)+len(fr.Data) > maxWSMessageBytes {
		return nil, 0, false, true
	}
	b.rxBuf = append(b.rxBuf, fr.Data...)
	if fr.Opcode != 0 {
		b.rxOpcode = int(fr.Opcode)
	}
	if !fr.Fin {
		return nil, 0, false, false
	}
	op := b.rxOpcode
	if op == 0 {
		op = websocket.TextMessage
	}
	payload = b.rxBuf
	b.rxBuf = nil
	b.rxOpcode = 0
	return payload, op, true, false
}

// wsResolveContainer resolves the one legitimate target for a proxied tab. It
// is a package var for the same reason wsDialContainer below is: the bridge
// tests substitute the dial, and this step reaches Docker too - it inspects the
// container via the daemon and refuses any non-private answer. Left
// un-substitutable it fails in every environment without the mc_<uuid> container
// (i.e. every test machine and every CI runner), so dialWSBridge would return
// before the stubbed dialer was ever called and a test waiting on that dial
// would wait forever.
var wsResolveContainer = containerAddr

// wsDialContainer dials the container WS, trying each candidate address in
// order and returning the first that upgrades. It is a package var so tests can
// substitute a controllable dialer; production uses gorilla with the 10s
// handshake timeout.
var wsDialContainer = func(addrs []string, path string, hdr http.Header) (*websocket.Conn, error) {
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	var lastErr error
	for _, addr := range addrs {
		c, _, err := dialer.Dial("ws://"+addr+path, hdr)
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("container ws unreachable")
	}
	return nil, lastErr
}

// splitWSFragments splits data into <=max byte fragments (reassembled in order
// on the far side). An empty message yields one empty fragment so it still
// transmits.
func splitWSFragments(data []byte, max int) [][]byte {
	if max <= 0 {
		max = wsFragmentSize
	}
	if len(data) == 0 {
		return [][]byte{{}}
	}
	var out [][]byte
	for off := 0; off < len(data); off += max {
		end := off + max
		if end > len(data) {
			end = len(data)
		}
		out = append(out, data[off:end])
	}
	return out
}

// handleWSOpen registers the bridge SYNCHRONOUSLY in a pending state, then
// dials the container WS in a background goroutine (WS5 I3). The blocking dial
// (up to the 10s handshake timeout) must not run on the shared per-Core read
// loop, or one slow/hung container would stall every other tenant's messages
// on this node. Registering the bridge before returning means inbound WsFrames
// for this request_id are buffered in bridge.inbound rather than dropped for
// arriving before the dial completes; they flush in order once the pumps start.
func (m *MeshManager) handleWSOpen(cc *coreConnection, reqID, serverUUID string, open *pb.WsOpen) {
	if open == nil || open.TargetPort < 1 || open.TargetPort > 65535 {
		cc.send(errorMsg(reqID, 502, "invalid ws target"))
		return
	}

	bridge := &wsBridge{owner: cc, server: serverUUID, inbound: make(chan *pb.NodeMessage, 64), done: make(chan struct{})}
	m.wsMu.Lock()
	perServer := 0
	for _, b := range m.wsBridges {
		if b.server == serverUUID {
			perServer++
		}
	}
	if len(m.wsBridges) >= maxWSBridges || perServer >= maxWSBridgesPerServer {
		m.wsMu.Unlock()
		// Bound concurrent bridges: reject beyond the cap with a policy WsClose
		// so a flood of WsOpens cannot exhaust this node and starve co-tenants.
		cc.send(&pb.NodeMessage{RequestId: reqID, Payload: &pb.NodeMessage_WsClose{WsClose: &pb.WsClose{Code: 1013, Reason: "node ws bridge limit reached"}}})
		return
	}
	m.wsBridges[reqID] = bridge
	m.wsMu.Unlock()

	go m.dialWSBridge(cc, reqID, serverUUID, open, bridge)
}

// dialWSBridge performs the blocking container WS dial off the read loop and,
// on success, activates the pending bridge and starts its pumps. On failure it
// reports a 502 and removes the pending bridge.
func (m *MeshManager) dialWSBridge(cc *coreConnection, reqID, serverUUID string, open *pb.WsOpen, bridge *wsBridge) {
	// Forward only sanitized request headers to the container upgrade.
	hdr := http.Header{}
	for _, kv := range nodeStripHopByHop(open.Headers) {
		// gorilla sets Upgrade/Connection/Sec-WebSocket-* itself; skip those.
		switch kv.Key {
		case "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Host":
			continue
		}
		hdr.Add(kv.Key, kv.Value)
	}

	// Same guard as the HTTP path: this URL is built by concatenation too, so a
	// path that is not origin-form re-targets the dial. See safeProxyPath.
	if !safeProxyPath(open.Path) {
		cc.send(errorMsg(reqID, 502, "invalid proxy path"))
		m.closeWSBridge(reqID)
		return
	}

	// The dialer takes a candidate list, but policy says there is exactly one
	// legitimate target: the server's own container. See containerAddr.
	addr, err := wsResolveContainer(serverUUID, int(open.TargetPort))
	if err != nil {
		cc.send(errorMsg(reqID, 502, err.Error()))
		m.closeWSBridge(reqID)
		return
	}
	conn, err := wsDialContainer([]string{addr}, open.Path, hdr)
	if err != nil {
		cc.send(errorMsg(reqID, 502, err.Error()))
		m.closeWSBridge(reqID) // drop the pending bridge
		return
	}

	// Activate under wsMu: attach the conn only if this bridge is still the
	// registered one. A teardown (inbound overflow, owner death, mesh shutdown)
	// may have removed and closed the pending bridge while we dialed - every
	// such path deletes from wsBridges under wsMu BEFORE calling bridge.close(),
	// so a still-present entry means bridge.close() has not read (nil) conn yet.
	// If it is gone, drop the freshly-dialed conn instead of leaking it.
	m.wsMu.Lock()
	if b, ok := m.wsBridges[reqID]; !ok || b != bridge {
		m.wsMu.Unlock()
		conn.Close()
		return
	}
	bridge.conn = conn
	m.wsMu.Unlock()

	m.startWSPumps(cc, reqID, serverUUID, bridge)
}

// startWSPumps launches the two bidirectional pumps once the bridge is active.
// Starting them only after activation guarantees queued inbound frames flush in
// arrival order and that no frame is written to a not-yet-dialed conn.
func (m *MeshManager) startWSPumps(cc *coreConnection, reqID, serverUUID string, bridge *wsBridge) {
	conn := bridge.conn

	// container -> Core pump (background goroutine)
	go func() {
		defer func() {
			cc.send(&pb.NodeMessage{RequestId: reqID, Payload: &pb.NodeMessage_WsClose{WsClose: &pb.WsClose{Code: 1000, Reason: ""}}})
			m.closeWSBridge(reqID)
		}()
		// One message is read whole into memory; unbounded, a container's
		// single multi-gigabyte message took the node agent down for every
		// tenant on it.
		conn.SetReadLimit(maxWSMessageBytes)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frags := splitWSFragments(data, wsFragmentSize)
			for i, f := range frags {
				if serr := cc.send(&pb.NodeMessage{
					RequestId: reqID,
					Payload:   &pb.NodeMessage_WsFrame{WsFrame: &pb.WsFrame{Opcode: int32(mt), Data: f, Fin: i == len(frags)-1}},
				}); serr != nil {
					return
				}
			}
		}
	}()

	// Core -> container pump (background goroutine): consume routed inbound messages.
	go func() {
		for {
			select {
			case <-bridge.done:
				return
			case msg := <-bridge.inbound:
				if cl := msg.GetWsClose(); cl != nil {
					m.closeWSBridge(reqID)
					return
				}
				fr := msg.GetWsFrame()
				if fr == nil {
					continue
				}
				payload, op, flush, overflow := bridge.appendInbound(fr)
				if overflow {
					// Fix A: the reassembled message hit the cap. Drop this one
					// bridge with a policy WsClose (1009 "message too big")
					// instead of letting rxBuf grow until the node OOMs and
					// takes the mesh down for every co-located tenant.
					cc.send(&pb.NodeMessage{RequestId: reqID, Payload: &pb.NodeMessage_WsClose{WsClose: &pb.WsClose{Code: 1009, Reason: "ws message exceeds node reassembly cap"}}})
					m.closeWSBridge(reqID)
					return
				}
				if !flush {
					continue // wait for the last fragment
				}
				if err := conn.WriteMessage(op, payload); err != nil {
					m.closeWSBridge(reqID)
					return
				}
			}
		}
	}()
	log.Printf("gRPC Mesh: ws bridge opened (request_id=%s, server=%s)", reqID, serverUUID)
}

// routeWSInbound delivers a Core->node WsFrame/WsClose to the open bridge.
// The enqueue is non-blocking (WS5 I2-inbound): this runs on the single
// per-connection read loop shared by every other Core->node request on this
// connection (file browser, RCON, ...), so a slow/stalled container must
// never be able to stall it by filling the bridge's 64-slot buffer. If the
// buffer is full we drop that one bridge instead of blocking the read loop;
// the browser reconnects and a fresh bridge is opened.
func (m *MeshManager) routeWSInbound(reqID string, msg *pb.NodeMessage) {
	m.wsMu.Lock()
	bridge, ok := m.wsBridges[reqID]
	m.wsMu.Unlock()
	if !ok {
		return
	}
	select {
	case bridge.inbound <- msg:
	case <-bridge.done:
	default:
		// Buffer full and bridge not already closing: closeWSBridge does the
		// same atomic lookup-delete-under-wsMu + sync.Once close as every
		// other teardown path (see below), so this can never double-close a
		// bridge that a pump is concurrently tearing down itself.
		m.closeWSBridge(reqID)
	}
}

// closeWSBridge tears down and unregisters a bridge (idempotent).
func (m *MeshManager) closeWSBridge(reqID string) {
	m.wsMu.Lock()
	bridge, ok := m.wsBridges[reqID]
	if ok {
		delete(m.wsBridges, reqID)
	}
	m.wsMu.Unlock()
	if ok {
		bridge.close()
	}
}

// closeWSBridgesForConn closes and unregisters every bridge owned by cc
// (WS5 I1 reaper). Call this from the per-connection read-loop cleanup path
// when a Core connection dies, so an orphaned bridge (e.g. pump 1 blocked in
// ReadMessage on an idle silent container, pump 2 blocked in select) is torn
// down deterministically instead of leaking on every Core restart.
//
// Uses the same atomic lookup-delete-under-wsMu + sync.Once close as
// closeWSBridge: the map delete happens while holding wsMu, so of any
// concurrent deleters racing on the same reqID (this reaper, a pump's own
// closeWSBridge call, or closeAllWSBridges), exactly one observes the entry
// present and removes it; the others see it already gone and skip closing.
// bridge.close() is therefore invoked at most once per bridge through this
// map-ownership discipline, with wsBridge's own sync.Once as a second,
// independent guard against a double close/panic.
func (m *MeshManager) closeWSBridgesForConn(cc *coreConnection) {
	m.wsMu.Lock()
	var owned []*wsBridge
	for reqID, b := range m.wsBridges {
		if b.owner == cc {
			owned = append(owned, b)
			delete(m.wsBridges, reqID)
		}
	}
	m.wsMu.Unlock()
	for _, b := range owned {
		b.close()
	}
}

// closeAllWSBridges closes and unregisters every open bridge regardless of
// owner (WS5 I1 reaper). Used on full mesh shutdown (closeAll). Same
// exactly-once discipline as closeWSBridgesForConn.
func (m *MeshManager) closeAllWSBridges() {
	m.wsMu.Lock()
	owned := make([]*wsBridge, 0, len(m.wsBridges))
	for reqID, b := range m.wsBridges {
		owned = append(owned, b)
		delete(m.wsBridges, reqID)
	}
	m.wsMu.Unlock()
	for _, b := range owned {
		b.close()
	}
}
