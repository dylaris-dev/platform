package nodegrpc

import (
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// A node refuses a chunk it cannot write at once, before the caller has sent
// TransferDone and started waiting. Registered before the chunks go out, the
// refusal is kept until the wait reads it.
func TestAwaitReplyKeepsAnEarlyRefusal(t *testing.T) {
	conn := &NodeConnection{pending: map[string]chan *pb.NodeMessage{}}
	wait, stop := conn.AwaitReply("w1")
	defer stop()
	if !conn.RouteResponse(&pb.NodeMessage{RequestId: "w1", Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 413}}}) {
		t.Fatal("the refusal found no listener")
	}
	resp, err := wait(time.Second)
	if err != nil || resp.GetError().GetCode() != 413 {
		t.Fatalf("resp %v err %v", resp, err)
	}
}

// A dropped connection or a node that never answers is an error, not success.
func TestAwaitReplyFailsWithoutAnAnswer(t *testing.T) {
	conn := &NodeConnection{pending: map[string]chan *pb.NodeMessage{}}
	wait, stop := conn.AwaitReply("w2")
	defer stop()
	if _, err := wait(20 * time.Millisecond); err == nil {
		t.Fatal("no answer was taken for success")
	}

	gone := &NodeConnection{} // pending nil: the connection is closed
	wait, stop = gone.AwaitReply("w3")
	defer stop()
	if _, err := wait(time.Second); err == nil {
		t.Fatal("a closed connection was taken for success")
	}
}

// stop unregisters, so a late reply is not delivered to nobody's channel.
func TestAwaitReplyStopUnregisters(t *testing.T) {
	conn := &NodeConnection{pending: map[string]chan *pb.NodeMessage{}}
	_, stop := conn.AwaitReply("w4")
	stop()
	if conn.RouteResponse(&pb.NodeMessage{RequestId: "w4"}) {
		t.Fatal("a reply was routed after stop")
	}
}
