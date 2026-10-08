package main

import (
	"errors"
	"sync"
	"time"

	pb "dylaris-proto/node"
)

// Credit-based flow control for streaming responses (NodeMessage.flow_window).
//
// Every response to every request shares one stream to Core and one read loop
// there, so a request whose reader is slower than this node used to fill its
// buffer on Core and stall that loop - and with it every other request to this
// node: consoles, file lists, tabs, commands. Core now opens a streaming
// request with a window; this node sends at most that many DataChunk / WsFrame
// messages for it ahead of Core's grants (FlowCredit), which Core issues only
// as the reader takes them. A slow reader slows only its own transfer.

// flowCreditWait is how long a transfer waits for its reader to take anything
// before it gives up. Generous on purpose: a reader that walked away is
// cancelled by Core at once (FlowCredit.cancel), so this only catches the
// case where that cancel never arrives, and a short limit would instead cut
// transfers to a reader that is merely slow behind a long request.
var flowCreditWait = 5 * time.Minute

// downloadsPerServer caps the downloads one server streams at once. They each
// run on their own goroutine now, so nothing else bounds them. Per server and
// not per node: a node-wide cap let one tenant's slow downloads queue every
// other tenant's reads - the editor, RCON's server.properties - behind them.
var downloadsPerServer = 4

// requestsPerServer caps the other file requests (list, copy, delete, hash,
// RCON...) one server runs at once off the read loop; see handleRequest.
var requestsPerServer = 4

// requestSlotWait is how long such a request waits for a slot before it is
// answered "busy" instead of run. Under Core's timeout for these requests.
var requestSlotWait = 20 * time.Second

// slotTable hands out one semaphore per server.
//
// ponytail: a server's entry is never removed; bounded by the servers this
// node has served since it started.
type slotTable struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

func (t *slotTable) of(serverUUID string, n int) chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.slots == nil {
		t.slots = map[string]chan struct{}{}
	}
	s, ok := t.slots[serverUUID]
	if !ok {
		s = make(chan struct{}, n)
		t.slots[serverUUID] = s
	}
	return s
}

var downloadSlots, requestSlots slotTable

func downloadSlotsFor(serverUUID string) chan struct{} {
	return downloadSlots.of(serverUUID, downloadsPerServer)
}

func requestSlotsFor(serverUUID string) chan struct{} {
	return requestSlots.of(serverUUID, requestsPerServer)
}

var errFlowStalled = errors.New("the reader took nothing for too long")

type flowCredit struct {
	mu     sync.Mutex
	n      uint32
	closed bool
	notify chan struct{} // capacity 1: "something changed"
}

func (f *flowCredit) acquire() error {
	timer := time.NewTimer(flowCreditWait)
	defer timer.Stop()
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return errors.New("the request ended")
		}
		if f.n > 0 {
			f.n--
			f.mu.Unlock()
			return nil
		}
		f.mu.Unlock()
		select {
		case <-f.notify:
		case <-timer.C:
			return errFlowStalled
		}
	}
}

func (f *flowCredit) add(k uint32) {
	f.mu.Lock()
	f.n += k
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *flowCredit) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

// openFlow starts flow control for a request Core opened with a window. A
// window of 0 (an older Core) leaves the request uncontrolled, as before.
func (cc *coreConnection) openFlow(reqID string, window uint32) {
	if window == 0 {
		return
	}
	cc.flowMu.Lock()
	defer cc.flowMu.Unlock()
	if cc.flows == nil {
		cc.flows = map[string]*flowCredit{}
	}
	cc.flows[reqID] = &flowCredit{n: window, notify: make(chan struct{}, 1)}
}

// closeFlow ends a request's flow control and releases a sender waiting on it.
func (cc *coreConnection) closeFlow(reqID string) {
	cc.flowMu.Lock()
	f := cc.flows[reqID]
	delete(cc.flows, reqID)
	cc.flowMu.Unlock()
	if f != nil {
		f.close()
	}
}

// cancelFlow ends a request whose reader on Core gave up. Unlike closeFlow it
// leaves the entry in place, so every further counted message of the request
// fails instead of going out uncontrolled; the request's own end removes it.
func (cc *coreConnection) cancelFlow(reqID string) {
	cc.flowMu.Lock()
	f := cc.flows[reqID]
	cc.flowMu.Unlock()
	if f != nil {
		f.close()
	}
}

// closeFlows releases every waiting sender when the connection ends.
func (cc *coreConnection) closeFlows() {
	cc.flowMu.Lock()
	flows := cc.flows
	cc.flows = nil
	cc.flowMu.Unlock()
	for _, f := range flows {
		f.close()
	}
}

// grantFlow applies Core's FlowCredit. A grant for a request that has ended
// is dropped.
func (cc *coreConnection) grantFlow(reqID string, k uint32) {
	cc.flowMu.Lock()
	f := cc.flows[reqID]
	cc.flowMu.Unlock()
	if f != nil {
		f.add(k)
	}
}

// awaitCredit blocks a counted message until its request may send it. Only
// DataChunk and WsFrame count; a request without a window is not held.
//
// A counted message of a controlled request goes out with flow_window set:
// that is how Core learns this node honours the window, and Core grants
// nothing to a node that has not said so (an older one would answer the grant
// with an error that ends the transfer).
func (cc *coreConnection) awaitCredit(msg *pb.NodeMessage) error {
	if msg.GetChunk() == nil && msg.GetWsFrame() == nil {
		return nil
	}
	cc.flowMu.Lock()
	f := cc.flows[msg.GetRequestId()]
	cc.flowMu.Unlock()
	if f == nil {
		return nil
	}
	if err := f.acquire(); err != nil {
		return err
	}
	msg.FlowWindow = 1
	return nil
}

// takeFlowCredit consumes a FlowCredit on the read loop: Core's reader took
// more of a streaming response, so the node may send that much more. It
// reports whether msg was one, so the loop does not hand it to the handler.
func (cc *coreConnection) takeFlowCredit(msg *pb.NodeMessage) bool {
	credit := msg.GetFlowCredit()
	if credit == nil {
		return false
	}
	if credit.Cancel {
		cc.cancelFlow(msg.RequestId)
	} else {
		cc.grantFlow(msg.RequestId, credit.Grant)
	}
	return true
}
