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

// SendRequestStreaming sends a message and returns a channel that will receive
// all response messages for this request_id (for chunked transfers).
// Caller MUST read from the channel until it's closed.
func (r *Registry) SendRequestStreaming(nodeID int, msg *pb.NodeMessage) (<-chan *pb.NodeMessage, error) {
	conn, ok := r.GetConnection(nodeID)
	if !ok {
		return nil, fmt.Errorf("node %d not connected", nodeID)
	}

	ch := make(chan *pb.NodeMessage, 64)
	conn.mu.Lock()
	if conn.pending == nil {
		conn.mu.Unlock()
		return nil, fmt.Errorf("node %d connection closed", nodeID)
	}
	conn.pending[msg.RequestId] = ch
	conn.mu.Unlock()

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
		return nil, fmt.Errorf("failed to send to node %d: %w", nodeID, err)
	}
	return ch, nil
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

// RouteResponse delivers an incoming message to the waiting handler via request_id.
// Returns false if no handler is waiting (message is dropped).
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
// non-blocking form: it either lands in the buffered channel or falls to
// default immediately, so this never sleeps while holding the lock.
func (conn *NodeConnection) RouteResponse(msg *pb.NodeMessage) bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	ch, ok := conn.pending[msg.RequestId]
	if !ok || ch == nil {
		return false
	}

	select {
	case ch <- msg:
		return true
	default:
		return false
	}
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
