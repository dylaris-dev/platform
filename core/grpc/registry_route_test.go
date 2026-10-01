package nodegrpc

import (
	"testing"
	"time"

	pb "dylaris-proto/node"
)

func chunkMsg(id string, off int64) *pb.NodeMessage {
	return &pb.NodeMessage{RequestId: id, Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Offset: off}}}
}

// A full channel dropped the chunk: a download to a browser slower than the
// node lost every piece past the buffer and still finished as complete.
func TestAFullChannelWaitsForTheReaderInsteadOfDroppingAChunk(t *testing.T) {
	ch := make(chan *pb.NodeMessage, 2)
	conn := &NodeConnection{pending: map[string]chan *pb.NodeMessage{"r": ch}}

	go func() {
		for i := int64(0); i < 10; i++ {
			conn.RouteResponse(chunkMsg("r", i))
		}
		conn.CloseStreamingRequest("r")
	}()
	var got []int64
	for m := range ch {
		got = append(got, m.GetChunk().Offset)
		time.Sleep(5 * time.Millisecond) // a slow reader
	}
	if len(got) != 10 {
		t.Fatalf("read %v, want all ten chunks in order", got)
	}
	for i, off := range got {
		if off != int64(i) {
			t.Fatalf("read %v, want all ten chunks in order", got)
		}
	}
}

// A reader that takes nothing gets its transfer ENDED, without the final
// TransferDone, rather than a hole in it - and a reader that left ends the wait.
func TestAStalledReaderHasItsTransferEnded(t *testing.T) {
	defer func(w time.Duration) { routeWait = w }(routeWait)
	routeWait = 50 * time.Millisecond

	ch := make(chan *pb.NodeMessage, 1)
	conn := &NodeConnection{pending: map[string]chan *pb.NodeMessage{"r": ch}}
	if !conn.RouteResponse(chunkMsg("r", 0)) {
		t.Fatal("the first chunk had room")
	}
	if conn.RouteResponse(chunkMsg("r", 1)) {
		t.Fatal("a chunk nobody read was reported delivered")
	}
	<-ch
	if _, open := <-ch; open {
		t.Fatal("the stalled transfer was not ended")
	}

	ch2 := make(chan *pb.NodeMessage, 1)
	conn.pending["q"] = ch2
	ch2 <- chunkMsg("q", 0)
	routeWait = time.Minute
	go func() {
		time.Sleep(20 * time.Millisecond)
		conn.mu.Lock()
		delete(conn.pending, "q") // CleanupRequest
		conn.mu.Unlock()
	}()
	start := time.Now()
	if conn.RouteResponse(chunkMsg("q", 1)) || time.Since(start) > 5*time.Second {
		t.Fatal("a reader that left did not end the wait")
	}
}
