package nodegrpc

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	pb "dylaris-proto/node"
)

// NodeRequestHandler answers one node-initiated request. node is the identity
// the stream authenticated, which is the only identity a handler may trust:
// nothing in the payload says who is asking, and a handler that wants to know
// whether this node may touch a run or a restore checks it against node.ID.
//
// The node retries a request on another Core replica after a timeout or a lost
// connection, so the same request can reach a handler twice, possibly on two
// replicas at once. A handler must be safe to run again for the same request.
//
// The returned message is sent back with the request's request_id and the
// node_request flag set, whatever the handler put there. A failure the node
// should act on belongs in the response's own error field; a nil return is
// answered as a handler failure.
type NodeRequestHandler func(ctx context.Context, node Node, msg *pb.NodeMessage) *pb.NodeMessage

// OpError codes for a node request that was never served. The node treats
// every one of them as "try another replica", which is why a handler's own
// refusal travels in its typed response instead.
const (
	NodeRequestHandlerFailed = 500
	NodeRequestUnsupported   = 501
	NodeRequestBusy          = 503
)

// maxConcurrentNodeRequests bounds the handler goroutines one stream may hold.
// A BYON node is a customer's machine, and without a bound it could make Core
// start a goroutine, and whatever database work the handler does, per message.
// A node that hits it gets NodeRequestBusy at once rather than waiting.
const maxConcurrentNodeRequests = 16

// nodeRequestRate and nodeRequestBurst bound how often one node may START a
// request, which the per-stream bound above does not: a node that sends a
// request and waits for its answer before the next never holds more than one
// slot, and can still make Core list parts and query the database as fast as
// Core answers. Keyed by node ID on the registry rather than held by the
// stream, so reconnecting does not refill it.
//
// Far above what an upload needs: at 64 MiB parts in batches of four, even a
// 10 Gbit/s link asks about five times a second.
const (
	nodeRequestRate  = 10.0 // requests per second, sustained
	nodeRequestBurst = 40.0
)

// requestBucket is one node's token bucket; see nodeRequestRate.
type requestBucket struct {
	tokens float64
	last   time.Time
}

// allowNodeRequest takes a token from nodeID's bucket, reporting false when
// there is none.
func (r *Registry) allowNodeRequest(nodeID int, now time.Time) bool {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if r.requestBuckets == nil {
		r.requestBuckets = make(map[int]*requestBucket)
	}
	b, ok := r.requestBuckets[nodeID]
	if !ok {
		b = &requestBucket{tokens: nodeRequestBurst, last: now}
		r.requestBuckets[nodeID] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(nodeRequestBurst, b.tokens+elapsed.Seconds()*nodeRequestRate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// payloadOneof is the envelope's payload oneof, looked up once.
var payloadOneof = (&pb.NodeMessage{}).ProtoReflect().Descriptor().Oneofs().ByName("payload")

// payloadKind names the oneof field a message carries ("upload_part_urls_request"),
// or "" when it carries none - which is also what a kind newer than this
// Core's proto decodes to.
func payloadKind(msg *pb.NodeMessage) string {
	fd := msg.ProtoReflect().WhichOneof(payloadOneof)
	if fd == nil {
		return ""
	}
	return string(fd.Name())
}

// HandleNodeRequest registers the handler for one node-initiated request kind,
// named by its oneof field in node.proto. Register before StartGRPCServer; a
// kind with no handler is answered NodeRequestUnsupported.
//
// It panics on a name that is not a payload field, because a typo here would
// otherwise ship as a feature that answers "unsupported" forever.
func (r *Registry) HandleNodeRequest(kind string, h NodeRequestHandler) {
	if payloadOneof.Fields().ByName(protoreflect.Name(kind)) == nil {
		panic(fmt.Sprintf("nodegrpc: %q is not a NodeMessage payload field", kind))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodeRequests == nil {
		r.nodeRequests = make(map[string]NodeRequestHandler)
	}
	r.nodeRequests[kind] = h
}

func (r *Registry) nodeRequestHandler(kind string) NodeRequestHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.nodeRequests[kind]
}

// serveNodeRequest answers a node-initiated request without blocking the
// caller, which is the stream's read loop. That loop is also what routes the
// node's replies to Core-initiated requests - file chunks, tab-proxy streams,
// RCON results - so a handler that waits on the database or on object storage
// must not run on it, or every other exchange with that node would stall
// behind it. slots is the per-stream bound; see maxConcurrentNodeRequests.
func (r *Registry) serveNodeRequest(ctx context.Context, node Node, conn *NodeConnection, slots chan struct{}, msg *pb.NodeMessage) {
	if !r.allowNodeRequest(node.ID, time.Now()) {
		replyNodeRequest(conn, node, msg, nodeRequestError(NodeRequestBusy, "too many requests from this node"))
		return
	}
	kind := payloadKind(msg)
	h := r.nodeRequestHandler(kind)
	if h == nil {
		replyNodeRequest(conn, node, msg, nodeRequestError(NodeRequestUnsupported, fmt.Sprintf("this Core does not handle node request %q", kind)))
		return
	}
	select {
	case slots <- struct{}{}:
	default:
		replyNodeRequest(conn, node, msg, nodeRequestError(NodeRequestBusy, "too many requests from this node in flight"))
		return
	}
	go func() {
		defer func() { <-slots }()
		replyNodeRequest(conn, node, msg, runNodeRequestHandler(ctx, h, node, kind, msg))
	}()
}

// runNodeRequestHandler recovers a handler panic: it runs on its own goroutine,
// where nothing else would, and one bad request must not take Core down for
// every node connected to it.
func runNodeRequestHandler(ctx context.Context, h NodeRequestHandler, node Node, kind string, msg *pb.NodeMessage) (resp *pb.NodeMessage) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("gRPC: node request %s from node %d panicked (request_id=%s): %v", kind, node.ID, msg.RequestId, p)
			resp = nodeRequestError(NodeRequestHandlerFailed, "the handler failed")
		}
	}()
	resp = h(ctx, node, msg)
	if resp == nil {
		resp = nodeRequestError(NodeRequestHandlerFailed, "the handler returned no response")
	}
	return resp
}

func replyNodeRequest(conn *NodeConnection, node Node, req, resp *pb.NodeMessage) {
	resp.RequestId = req.RequestId
	resp.NodeRequest = true
	if err := conn.Send(resp); err != nil {
		log.Printf("gRPC: could not answer node %d's request (request_id=%s): %v", node.ID, req.RequestId, err)
	}
}

func nodeRequestError(code int32, message string) *pb.NodeMessage {
	return &pb.NodeMessage{Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: code, Message: message}}}
}
