package nodegrpc

import (
	"runtime"
	"sync"
	"testing"
	"time"

	pb "dylaris-proto/node"

	"google.golang.org/grpc"
)

// lockedStream records what Core sends a node; the flow pump sends from its
// own goroutine.
type lockedStream struct {
	grpc.ServerStream
	mu   sync.Mutex
	sent []*pb.NodeMessage
}

func (s *lockedStream) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

func (s *lockedStream) Recv() (*pb.NodeMessage, error) { select {} }

func (s *lockedStream) grants(requestID string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n uint32
	for _, m := range s.sent {
		if c := m.GetFlowCredit(); c != nil && m.RequestId == requestID {
			n += c.Grant
		}
	}
	return n
}

func (s *lockedStream) request(requestID string) *pb.NodeMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.sent {
		if m.RequestId == requestID && m.GetFlowCredit() == nil {
			return m
		}
	}
	return nil
}

// markedChunk is a chunk from a node that honours the window.
func markedChunk(id string, off int64) *pb.NodeMessage {
	m := chunkMsg(id, off)
	m.FlowWindow = 1
	return m
}

func (s *lockedStream) cancels(requestID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.sent {
		if c := m.GetFlowCredit(); c != nil && c.Cancel && m.RequestId == requestID {
			n++
		}
	}
	return n
}

func flowRegistry(t *testing.T) (*Registry, *NodeConnection, *lockedStream) {
	t.Helper()
	st := &lockedStream{}
	r := NewRegistry()
	conn := r.Register(7, "tok", st)
	return r, conn, st
}

// The node is told the window, and granted more only as the reader takes
// messages - never ahead of it. A reader that takes nothing is granted
// nothing, so a node that honours the window stops after it and never fills
// the buffer that used to stall the read loop for every other request.
func TestCreditIsGrantedOnlyAsTheReaderTakes(t *testing.T) {
	r, conn, st := flowRegistry(t)
	ch, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "dl", Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{}}})
	if err != nil {
		t.Fatal(err)
	}
	if req := st.request("dl"); req == nil || req.FlowWindow != flowWindow {
		t.Fatalf("the request did not open a flow window: %+v", req)
	}

	for i := int64(0); i < flowWindow; i++ {
		if !conn.RouteResponse(markedChunk("dl", i)) {
			t.Fatalf("chunk %d of the window was not accepted", i)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if g := st.grants("dl"); g != 0 {
		t.Fatalf("granted %d before the reader took anything", g)
	}

	for i := 0; i < flowGrantEvery; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("the reader got nothing")
		}
	}
	deadline := time.Now().Add(time.Second)
	for st.grants("dl") != flowGrantEvery && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if g := st.grants("dl"); g != flowGrantEvery {
		t.Fatalf("granted %d after the reader took %d, want %d", g, flowGrantEvery, flowGrantEvery)
	}
}

// A full window from a request nobody reads must not hold up another request
// to the same node. Within the window, routing never waits.
func TestAStalledRequestDoesNotHoldUpAnother(t *testing.T) {
	r, conn, _ := flowRegistry(t)
	if _, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "slow"}); err != nil {
		t.Fatal(err)
	}
	fast, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := int64(0); i < flowWindow; i++ {
		conn.RouteResponse(chunkMsg("slow", i))
	}
	conn.RouteResponse(chunkMsg("fast", 0))
	select {
	case <-fast:
	case <-time.After(time.Second):
		t.Fatal("the other request's message did not arrive")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("routing past a stalled request took %v", d)
	}
}

// A reader that gives up (CleanupRequest) ends its pump; nothing waits on it.
func TestAnAbandonedReaderLeavesNoPump(t *testing.T) {
	r, conn, _ := flowRegistry(t)
	before := runtime.NumGoroutine()
	ch, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	// The reader takes everything there is and THEN gives up, so the pump is
	// waiting on the node, not on the reader, when it is told to stop.
	conn.RouteResponse(chunkMsg("gone", 0))
	conn.RouteResponse(chunkMsg("gone", 1))
	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("the reader got nothing")
		}
	}
	time.Sleep(20 * time.Millisecond)
	r.CleanupRequest(7, "gone")
	deadline := time.Now().Add(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto closed
			}
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the reader's channel never closed after it gave up")
		}
	}
closed:
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines %d > %d: the pump outlived its reader", n, before)
	}
}

// An older node answers a FlowCredit it cannot parse with an error under the
// same request_id, which ends the transfer. A node that never marks its
// chunks is never granted anything.
func TestAnOldNodeIsNeverSentCredit(t *testing.T) {
	r, conn, st := flowRegistry(t)
	ch, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "old"})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 2*flowGrantEvery; i++ {
		conn.RouteResponse(chunkMsg("old", i))
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("the reader got nothing")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if g := st.grants("old"); g != 0 {
		t.Fatalf("an old node was granted %d", g)
	}
}

// A transfer that ended is not cancelled: the reader's CleanupRequest races
// the pump seeing the channel close, and an old node would answer the cancel
// with an error line in its log for every finished download.
func TestAFinishedTransferIsNotCancelled(t *testing.T) {
	r, conn, st := flowRegistry(t)
	if _, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "dl"}); err != nil {
		t.Fatal(err)
	}
	// The last chunk fills the reader's slot, so the pump is left holding the
	// final TransferDone when the reader gives up: a transfer that is over.
	conn.RouteResponse(markedChunk("dl", 0))
	conn.RouteResponse(&pb.NodeMessage{RequestId: "dl", Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: 1}}})
	time.Sleep(20 * time.Millisecond)
	r.CleanupRequest(7, "dl")
	time.Sleep(50 * time.Millisecond)
	if n := st.cancels("dl"); n != 0 {
		t.Fatalf("a finished transfer was cancelled %d times", n)
	}
}

// A reader that gives up tells a node that honours the window to stop, so the
// transfer ends at once instead of waiting out the node's stall timeout.
func TestGivingUpCancelsTheTransfer(t *testing.T) {
	r, _, st := flowRegistry(t)
	if _, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "dl"}); err != nil {
		t.Fatal(err)
	}
	r.CleanupRequest(7, "dl") // before the node sent anything: a slot it waits for
	deadline := time.Now().Add(time.Second)
	for st.cancels("dl") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := st.cancels("dl"); n != 1 {
		t.Fatalf("%d cancels sent, want 1", n)
	}
}

// The pump of a reader that stopped reading waits on that reader. After the
// node reconnected, CleanupRequest finds the NEW connection, and a stop kept
// on the old one was never found: the pump waited for good.
func TestGivingUpAfterAReconnectEndsThePump(t *testing.T) {
	r, conn, _ := flowRegistry(t)
	ch, err := r.SendRequestStreaming(7, &pb.NodeMessage{RequestId: "dl"})
	if err != nil {
		t.Fatal(err)
	}
	// Nobody reads: the first chunk fills the reader's slot, the pump holds
	// the second.
	conn.RouteResponse(markedChunk("dl", 0))
	conn.RouteResponse(markedChunk("dl", 1))
	time.Sleep(20 * time.Millisecond)
	r.Register(7, "tok", &lockedStream{})
	r.CleanupRequest(7, "dl")
	<-ch // the one that made it to the slot
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("the pump handed on a message after the reader gave up")
		}
	case <-time.After(time.Second):
		t.Fatal("the pump outlived a reader that gave up after a reconnect")
	}
}
