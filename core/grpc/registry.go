package nodegrpc

import (
	"fmt"
	"sync"
	"time"

	pb "dylaris-proto/node"
)

// NodeConnection represents an active gRPC stream to a single Node.
type NodeConnection struct {
	NodeID    int
	NodeToken string
	Stream    pb.NodeService_NodeConnectServer

	// pending maps request_id → response channel.
	// The stream reader goroutine routes incoming messages to the correct waiter.
	pending map[string]chan *pb.NodeMessage
	mu      sync.Mutex
	sendMu  sync.Mutex // serializes Stream.Send across concurrent handlers + WS pumps
}

// Registry is thread-safe for concurrent access from HTTP handlers.
type Registry struct {
	connections map[int]*NodeConnection
	mu          sync.RWMutex
	// nodeRequests maps a payload kind to the handler for requests nodes start;
	// see node_request.go.
	nodeRequests map[string]NodeRequestHandler

	// requestBuckets rate-limits node-initiated requests per node ID; see
	// allowNodeRequest. Its own lock, so the limiter never waits on mu.
	limitMu        sync.Mutex
	requestBuckets map[int]*requestBucket

	// flowStops maps a streaming request_id to the channel that stops its
	// pump. Kept here and not on the connection: CleanupRequest finds the
	// CURRENT connection, and after a reconnect that is not the one the
	// request was opened on - a stop kept there was never found, and the pump
	// of a reader that gave up waited on it for good.
	flowMu    sync.Mutex
	flowStops map[string]chan struct{}
}

func NewRegistry() *Registry {
	return &Registry{
		connections: make(map[int]*NodeConnection),
	}
}

// Register adds a new Node connection to the registry.
func (r *Registry) Register(nodeID int, token string, stream pb.NodeService_NodeConnectServer) *NodeConnection {
	conn := &NodeConnection{
		NodeID:    nodeID,
		NodeToken: token,
		Stream:    stream,
		pending:   make(map[string]chan *pb.NodeMessage),
	}

	r.mu.Lock()
	if old, ok := r.connections[nodeID]; ok {
		old.mu.Lock()
		for _, ch := range old.pending {
			close(ch)
		}
		old.pending = nil
		old.mu.Unlock()
	}
	r.connections[nodeID] = conn
	r.mu.Unlock()

	return conn
}

// Unregister removes a Node connection from the registry - but only when the
// registry still holds THAT connection.
//
// The identity check is the whole point. Each NodeConnect stream defers this
// with its own node id, and a node can be registered twice for a while: gRPC
// keepalive gives Core up to about 15 seconds to notice a stream is dead
// (Time 5s + Timeout 10s), and a node whose network blipped, or which was
// restarted, reconnects well inside that. Register handles the overlap
// correctly - it closes the superseded connection's pending channels and takes
// over the map entry. What did not was the LATE teardown that follows: when the
// old stream's Recv finally errored, its deferred Unregister deleted whatever
// was under that node id, which by then was the NEW connection.
//
// The node was then connected and Core believed it was not. Every command,
// file transfer and tab-proxy request to it failed with "node not connected"
// until the live stream itself died and the node reconnected a second time -
// and nothing in either log said why, because both halves had done exactly what
// they were told.
//
// A superseded connection needs no cleanup here; Register already closed its
// pending channels and nil'd the map, so returning early is complete, not a
// shortcut. This is the same reconnect overlap TestRouteResponseSurvivesAReconnectRace
// covers for pending channels, applied to the registry entry itself.
func (r *Registry) Unregister(nodeID int, conn *NodeConnection) {
	r.mu.Lock()
	if cur, ok := r.connections[nodeID]; ok && cur == conn {
		cur.mu.Lock()
		for _, ch := range cur.pending {
			close(ch)
		}
		cur.pending = nil
		cur.mu.Unlock()
		delete(r.connections, nodeID)
	}
	r.mu.Unlock()
}

// GetConnection returns the active connection for a Node, if any.
func (r *Registry) GetConnection(nodeID int) (*NodeConnection, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conn, ok := r.connections[nodeID]
	return conn, ok
}

// IsConnected returns true if a Node has an active gRPC connection.
func (r *Registry) IsConnected(nodeID int) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.connections[nodeID]
	return ok
}

// SendRequest sends a message over the gRPC stream and waits for a response
// matching the same request_id. Returns error on timeout or if Node is not connected.
func (r *Registry) SendRequest(nodeID int, msg *pb.NodeMessage, timeout time.Duration) (*pb.NodeMessage, error) {
	conn, ok := r.GetConnection(nodeID)
	if !ok {
		return nil, fmt.Errorf("node %d not connected", nodeID)
	}

	ch := make(chan *pb.NodeMessage, 1)
	conn.mu.Lock()
	if conn.pending == nil {
		conn.mu.Unlock()
		return nil, fmt.Errorf("node %d connection closed", nodeID)
	}
	conn.pending[msg.RequestId] = ch
	conn.mu.Unlock()

	defer func() {
		conn.mu.Lock()
		delete(conn.pending, msg.RequestId)
		conn.mu.Unlock()
	}()

	if err := conn.Send(msg); err != nil {
		return nil, fmt.Errorf("failed to send to node %d: %w", nodeID, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("node %d connection closed while waiting", nodeID)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response from node %d (request %s)", nodeID, msg.RequestId)
	}
}

// flowWindow is how many DataChunk / WsFrame messages of one streaming request
// a node may have in flight, and flowGrantEvery how many the reader takes
// before Core grants that many again. The window is below the 64-message
// buffer, so a node that honours it never fills the buffer and RouteResponse
// never waits: one slow reader no longer stalls the node's read loop, and with
// it every other request to that node.
const (
	flowWindow     = 32
	flowGrantEvery = 16
)

// SendRequestStreaming sends a message and returns a channel that will receive
// all response messages for this request_id (for chunked transfers).
// Caller MUST read from the channel until it's closed, or call CleanupRequest.
//
// The request carries flow_window, and the returned channel is fed by a pump
// that grants the node more only as the caller reads (FlowCredit). An older
// node ignores the window and is served as before: the buffer fills and
// RouteResponse waits for the reader.
func (r *Registry) SendRequestStreaming(nodeID int, msg *pb.NodeMessage) (<-chan *pb.NodeMessage, error) {
	conn, ok := r.GetConnection(nodeID)
	if !ok {
		return nil, fmt.Errorf("node %d not connected", nodeID)
	}

	ch := make(chan *pb.NodeMessage, 64)
	out := make(chan *pb.NodeMessage, 1)
	stop := make(chan struct{})
	conn.mu.Lock()
	if conn.pending == nil {
		conn.mu.Unlock()
		return nil, fmt.Errorf("node %d connection closed", nodeID)
	}
	conn.pending[msg.RequestId] = ch
	conn.mu.Unlock()
	r.flowMu.Lock()
	if r.flowStops == nil {
		r.flowStops = map[string]chan struct{}{}
	}
	r.flowStops[msg.RequestId] = stop
	r.flowMu.Unlock()
	msg.FlowWindow = flowWindow

	// Send request
	if err := conn.Send(msg); err != nil {
		// Close under the lock, and only if the entry is still ours. The node's
		// stream goroutine can run Unregister (or a reconnect can run Register)
		// concurrently with this send failing, and that path closes ch and nils
		// the map while holding conn.mu. Closing here without the lock, or
		// without re-checking, races it into a close-of-closed-channel panic -
		// the same window closed in RouteResponse, on the sibling path. A nil
		// map yields (nil, false), so a delete-after-Unregister is a no-op and
		// the already-closed channel is not touched again.
		conn.mu.Lock()
		if _, ok := conn.pending[msg.RequestId]; ok {
			delete(conn.pending, msg.RequestId)
			close(ch)
		}
		conn.mu.Unlock()
		r.stopFlow(msg.RequestId)
		return nil, fmt.Errorf("failed to send to node %d: %w", nodeID, err)
	}
	go conn.pumpFlow(msg.RequestId, ch, out, stop)
	return out, nil
}

// pumpFlow hands the node's messages to the reader one at a time and grants
// the node more as the reader takes them. It ends when the request's channel
// closes (the transfer ended or the connection went) or the reader gives up
// (CleanupRequest), so an abandoned reader leaves no goroutine behind; the
// node is then told to stop at once rather than wait out its stall timeout
// with a download slot held. The cancel goes out even before the node has
// shown it honours the window - a transfer still waiting for a slot has sent
// nothing yet - and an older node's "unknown request type" answer to it finds
// nobody waiting. Only a transfer that already ended is not cancelled.
//
// Credit goes only to a node that has shown it honours the window, by
// marking its counted messages with flow_window. An older node answers a
// FlowCredit it cannot parse with "unknown request type" under this very
// request_id - an error the reader takes as the end of the transfer - so
// granting to it cut every tab page past 1 MB and every websocket tab.
func (conn *NodeConnection) pumpFlow(requestID string, in <-chan *pb.NodeMessage, out chan<- *pb.NodeMessage, stop <-chan struct{}) {
	defer close(out)
	taken := uint32(0)
	honours, ended := false, false
	cancel := func() {
		if !ended {
			_ = conn.Send(&pb.NodeMessage{RequestId: requestID, Payload: &pb.NodeMessage_FlowCredit{FlowCredit: &pb.FlowCredit{Cancel: true}}})
		}
	}
	for {
		// Stop is watched while WAITING for the node too, not only while
		// handing a message over: a reader that gave up between two messages
		// would otherwise leave this goroutine waiting on a channel nothing
		// will ever close.
		var msg *pb.NodeMessage
		select {
		case m, ok := <-in:
			if !ok {
				return
			}
			msg = m
			ended = IsFinalTransferDone(msg) || msg.GetError() != nil || msg.GetWsClose() != nil
		case <-stop:
			cancel()
			return
		}
		select {
		case out <- msg:
		case <-stop:
			cancel()
			return
		}
		if msg.GetChunk() == nil && msg.GetWsFrame() == nil {
			continue // only these count against the window
		}
		if msg.FlowWindow != 0 {
			honours = true
		}
		if !honours {
			continue
		}
		if taken++; taken >= flowGrantEvery {
			// A failed grant means the stream is going; its close ends this loop.
			_ = conn.Send(&pb.NodeMessage{RequestId: requestID, Payload: &pb.NodeMessage_FlowCredit{FlowCredit: &pb.FlowCredit{Grant: taken}}})
			taken = 0
		}
	}
}

// stopFlow ends a request's pump, on whichever connection it was opened.
func (r *Registry) stopFlow(requestID string) {
	r.flowMu.Lock()
	stop, ok := r.flowStops[requestID]
	delete(r.flowStops, requestID)
	r.flowMu.Unlock()
	if ok {
		close(stop)
	}
}

// SendOnStream sends one message to a node's stream out-of-band, without
// registering a pending waiter. Used for follow-up messages on an already-open
// streaming request (e.g. a WsFrame carrying browser->container bytes).
func (r *Registry) SendOnStream(nodeID int, msg *pb.NodeMessage) error {
	conn, ok := r.GetConnection(nodeID)
	if !ok {
		return fmt.Errorf("node %d not connected", nodeID)
	}
	return conn.Send(msg)
}

// CleanupRequest removes a pending request channel (used after streaming is done).
func (r *Registry) CleanupRequest(nodeID int, requestID string) {
	r.stopFlow(requestID)
	conn, ok := r.GetConnection(nodeID)
	if !ok {
		return
	}
	conn.mu.Lock()
	delete(conn.pending, requestID)
	conn.mu.Unlock()
}

// Send serializes writes to the underlying gRPC stream. gRPC streams are not
// safe for concurrent Send; the WS bridge and parallel file ops can both write.
func (conn *NodeConnection) Send(msg *pb.NodeMessage) error {
	conn.sendMu.Lock()
	defer conn.sendMu.Unlock()
	return conn.Stream.Send(msg)
}

// routeWait is how long RouteResponse waits for a reader to make room, and
// routePoll how often it looks.
var (
	routeWait = 30 * time.Second
	routePoll = 2 * time.Millisecond
)

// IsFinalTransferDone reports whether msg is the TransferDone that ends a
// chunked transfer. The metadata TransferDone sent BEFORE the chunks has a
// Filename and TotalBytes==0; the final one has no Filename, or TotalBytes>0
// (a zip names its file in both). A transfer whose channel closes without this
// message did not complete.
func IsFinalTransferDone(msg *pb.NodeMessage) bool {
	done := msg.GetTransferDone()
	return done != nil && (done.TotalBytes > 0 || done.Filename == "")
}

// RouteResponse delivers an incoming message to the waiting handler via request_id.
// Returns false if no handler is waiting (message is dropped).
//
// A full channel is waited on, not skipped. It used to drop the message, and a
// chunk is a piece of a file: a download to a browser slower than the node
// lost every chunk past the 64 buffered ones and still completed with 200,
// a corrupt file. Waiting slows this node's read loop to the slow reader's
// pace, which is what TCP would do. A reader that takes nothing for routeWait
// has its transfer ENDED instead - closed without the final TransferDone,
// which every consumer reads as incomplete.
//
// The lock is held ACROSS the send, not just across the map lookup. Every path
// that closes a pending channel (Register replacing a reconnecting node's old
// connection, Unregister, CloseStreamingRequest) closes it and drops the map
// entry while holding this same mutex, so holding it here means the channel
// this goroutine is about to send on cannot be closed underneath it. Looking
// the channel up under the lock and then sending after releasing it left a
// window in which a node reconnecting under the same id - which runs Register
// on the NEW stream's goroutine while the OLD stream's read loop is still
// routing - could close the channel between the two, and a send on a closed
// channel panics. Nothing in this package recovers that panic, so it would
// take the Core process down.
//
// Holding the mutex across the send is safe because the send is the
// non-blocking form; the wait for room happens with the lock RELEASED, and the
// entry is looked up again each time, so a reader that gave up (CleanupRequest)
// or a connection that closed ends the wait at once.
func (conn *NodeConnection) RouteResponse(msg *pb.NodeMessage) bool {
	deadline := time.Now().Add(routeWait)
	for {
		conn.mu.Lock()
		ch, ok := conn.pending[msg.RequestId]
		if !ok || ch == nil {
			conn.mu.Unlock()
			return false
		}
		select {
		case ch <- msg:
			conn.mu.Unlock()
			return true
		default:
		}
		if time.Now().After(deadline) {
			close(ch)
			delete(conn.pending, msg.RequestId)
			conn.mu.Unlock()
			return false
		}
		conn.mu.Unlock()
		time.Sleep(routePoll)
	}
}

// AwaitReply registers for the one reply a request will get LATER, after the
// caller has streamed to the node with plain Sends. A file write is
// WriteReq -> DataChunk* -> TransferDone, and the node answers the
// TransferDone with the outcome of committing the file - or answers a chunk it
// could not write with an error at once. Nobody was listening for either: the
// write was reported saved while the node had refused it (a full disk quota,
// a failed rename), and the user found out when the file was not there.
//
// Register BEFORE the chunks go out, so a mid-stream refusal is caught too.
// wait returns the reply or an error on timeout or a dropped connection; stop
// unregisters and must be deferred.
func (conn *NodeConnection) AwaitReply(requestID string) (wait func(time.Duration) (*pb.NodeMessage, error), stop func()) {
	ch := make(chan *pb.NodeMessage, 1)
	conn.mu.Lock()
	registered := conn.pending != nil
	if registered {
		conn.pending[requestID] = ch
	}
	conn.mu.Unlock()
	stop = func() {
		conn.mu.Lock()
		if conn.pending != nil && conn.pending[requestID] == ch {
			delete(conn.pending, requestID)
		}
		conn.mu.Unlock()
	}
	wait = func(timeout time.Duration) (*pb.NodeMessage, error) {
		if !registered {
			return nil, fmt.Errorf("node connection closed")
		}
		select {
		case resp, ok := <-ch:
			if !ok {
				return nil, fmt.Errorf("node connection closed while waiting")
			}
			return resp, nil
		case <-time.After(timeout):
			return nil, fmt.Errorf("timeout waiting for the node to confirm (request %s)", requestID)
		}
	}
	return wait, stop
}

// CloseStreamingRequest closes the channel for a specific request_id
// (called when TransferDone is received to signal end of chunked transfer).
func (conn *NodeConnection) CloseStreamingRequest(requestID string) {
	conn.mu.Lock()
	if ch, ok := conn.pending[requestID]; ok {
		close(ch)
		delete(conn.pending, requestID)
	}
	conn.mu.Unlock()
}
