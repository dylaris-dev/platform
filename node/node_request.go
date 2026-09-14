package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	pb "dylaris-proto/node"
)

// errNoCoreConnection is returned when there is no authenticated Core stream
// left to try.
var errNoCoreConnection = errors.New("no connection to Core")

// Request sends a node-initiated request to Core and waits for its answer. It
// is the one way a node ASKS Core for something on the control stream; every
// other exchange there is Core asking the node.
//
// One attempt goes to one Core replica. If that attempt gets no answer within
// attemptTimeout, loses its connection, or is answered with an OpError (Core
// did not serve it: unsupported kind, busy, handler failure), the request is
// sent once more to a DIFFERENT replica, and that answer is final. The retry
// is what keeps a rollout from hanging a backup: an old Core does not know the
// node_request flag, logs the message as unroutable and never answers, so only
// the timeout ends that attempt, and the replica next to it may already be new.
// It also means a Core handler can see the same request twice.
//
// A typed response is returned as it is, including one whose error field is
// set: that is Core's decision about the request, and asking another replica
// would get the same one.
func (m *MeshManager) Request(ctx context.Context, req *pb.NodeMessage, attemptTimeout time.Duration) (*pb.NodeMessage, error) {
	req.NodeRequest = true
	if req.RequestId == "" {
		req.RequestId = newNodeRequestID()
	}
	var tried []string
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		coreID, cc := m.pickCoreConnection(tried)
		if cc == nil {
			if lastErr == nil {
				return nil, errNoCoreConnection
			}
			return nil, fmt.Errorf("node request %s: no other Core to retry on: %w", req.RequestId, lastErr)
		}
		tried = append(tried, coreID)
		resp, err := cc.request(ctx, req, attemptTimeout)
		if err == nil {
			return resp, nil
		}
		lastErr = fmt.Errorf("Core %s: %w", coreID, err)
		if ctx.Err() != nil {
			return nil, lastErr
		}
		log.Printf("gRPC Mesh: node request %s failed on Core %s: %v", req.RequestId, coreID, err)
	}
	return nil, fmt.Errorf("node request %s: %w", req.RequestId, lastErr)
}

// pickCoreConnection returns a connected Core not in exclude. Map order is
// random, which spreads requests across replicas.
func (m *MeshManager) pickCoreConnection(exclude []string) (string, *coreConnection) {
	m.mu.Lock()
	defer m.mu.Unlock()
next:
	for id, cc := range m.connections {
		for _, e := range exclude {
			if e == id {
				continue next
			}
		}
		return id, cc
	}
	return "", nil
}

// request is one attempt on this connection.
func (cc *coreConnection) request(ctx context.Context, req *pb.NodeMessage, timeout time.Duration) (*pb.NodeMessage, error) {
	ch := make(chan *pb.NodeMessage, 1)
	cc.pendingMu.Lock()
	if cc.pending == nil {
		cc.pendingMu.Unlock()
		return nil, errors.New("connection closed")
	}
	cc.pending[req.RequestId] = ch
	cc.pendingMu.Unlock()
	defer func() {
		cc.pendingMu.Lock()
		delete(cc.pending, req.RequestId)
		cc.pendingMu.Unlock()
	}()

	if err := cc.send(req); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("connection closed while waiting for the answer")
		}
		if e := resp.GetError(); e != nil {
			return nil, fmt.Errorf("not served (%d): %s", e.Code, e.Message)
		}
		return resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("no answer within %s", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// routeNodeResponse hands Core's answer to the waiting Request. Called from the
// read loop for every message carrying the node_request flag, and only those:
// such a message is never a request for the node to handle.
//
// The send happens under pendingMu, the lock closePending closes the channels
// under, so it can never hit a closed channel. It is non-blocking, so holding
// the lock never waits: the buffer holds one answer, and a duplicate is dropped.
func (cc *coreConnection) routeNodeResponse(msg *pb.NodeMessage) {
	cc.pendingMu.Lock()
	defer cc.pendingMu.Unlock()
	ch, ok := cc.pending[msg.RequestId]
	if !ok {
		// Usually the answer to an attempt that already timed out.
		log.Printf("gRPC Mesh: answer to a node request nobody is waiting for (request_id=%s)", msg.RequestId)
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

// closePending fails every Request waiting on this connection at once, instead
// of letting each run out its timeout against a stream that is gone.
func (cc *coreConnection) closePending() {
	cc.pendingMu.Lock()
	defer cc.pendingMu.Unlock()
	for _, ch := range cc.pending {
		close(ch)
	}
	cc.pending = nil
}

func newNodeRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return "node-" + hex.EncodeToString(b)
}
