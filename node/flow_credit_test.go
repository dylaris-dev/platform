package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

func chunkFor(id string) *pb.NodeMessage {
	return &pb.NodeMessage{RequestId: id, Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{}}}
}

// The node may send a window's worth ahead of Core's grants and no more; a
// grant lets exactly that much more through.
func TestASenderStopsAtTheWindowUntilCoreGrants(t *testing.T) {
	cc := &coreConnection{}
	cc.openFlow("dl", 2)
	for i := 0; i < 2; i++ {
		if err := cc.awaitCredit(chunkFor("dl")); err != nil {
			t.Fatalf("chunk %d inside the window: %v", i, err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- cc.awaitCredit(chunkFor("dl")) }()
	select {
	case err := <-done:
		t.Fatalf("a chunk past the window went out without a grant (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	cc.grantFlow("dl", 1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("granted chunk: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a grant did not release the waiting chunk")
	}
}

// A reader that takes nothing for flowCreditWait ends the transfer; the
// request ending, or the connection, releases a waiting sender at once.
func TestAStalledOrEndedTransferReleasesItsSender(t *testing.T) {
	prev := flowCreditWait
	flowCreditWait = 50 * time.Millisecond
	t.Cleanup(func() { flowCreditWait = prev })

	cc := &coreConnection{}
	cc.openFlow("stalled", 1)
	_ = cc.awaitCredit(chunkFor("stalled"))
	if err := cc.awaitCredit(chunkFor("stalled")); !errors.Is(err, errFlowStalled) {
		t.Fatalf("err = %v, want the stall", err)
	}

	flowCreditWait = time.Minute
	cc.openFlow("ended", 1)
	_ = cc.awaitCredit(chunkFor("ended"))
	done := make(chan error, 1)
	go func() { done <- cc.awaitCredit(chunkFor("ended")) }()
	time.Sleep(20 * time.Millisecond)
	cc.closeFlows()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a sender was let through by the connection ending")
		}
	case <-time.After(time.Second):
		t.Fatal("the connection ending did not release the waiting sender")
	}
}

// Only DataChunk and WsFrame count, and only for a request with a window: the
// response head, the end of a transfer, an error, and every request from an
// older Core (window 0) go out as before.
func TestOnlyCountedMessagesOfAControlledRequestWait(t *testing.T) {
	prev := flowCreditWait
	flowCreditWait = 50 * time.Millisecond
	t.Cleanup(func() { flowCreditWait = prev })

	cc := &coreConnection{}
	cc.openFlow("dl", 1)
	_ = cc.awaitCredit(chunkFor("dl")) // the window is spent

	for name, msg := range map[string]*pb.NodeMessage{
		"end of transfer": {RequestId: "dl", Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{}}},
		"an error":        errorMsg("dl", 500, "x"),
		"an old Core's":   chunkFor("no-window"),
	} {
		if err := cc.awaitCredit(msg); err != nil {
			t.Errorf("%s was held: %v", name, err)
		}
	}
	cc.openFlow("old-core", 0)
	if err := cc.awaitCredit(chunkFor("old-core")); err != nil {
		t.Errorf("a request without a window was held: %v", err)
	}
	ws := &pb.NodeMessage{RequestId: "dl", Payload: &pb.NodeMessage_WsFrame{WsFrame: &pb.WsFrame{}}}
	if err := cc.awaitCredit(ws); !errors.Is(err, errFlowStalled) {
		t.Errorf("a WsFrame past the window was not held: %v", err)
	}
}

func (f *fakeCoreStream) chunks(id string) int {
	n := 0
	for _, m := range f.messages() {
		if m.GetChunk() != nil && m.RequestId == id {
			n++
		}
	}
	return n
}

// A download ran on the node's shared read loop for its whole length, so the
// node read nothing else from Core meanwhile - and a download waiting for
// credit there could never read the grant it waits for. handleRequest must hand
// it off and return, and the grant must then move it on.
func TestADownloadDoesNotHoldTheReadLoop(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	h := NewStreamHandler(sm)
	const uuid = "33333333-3333-3333-3333-333333333333"
	root := sm.GetServerDir(uuid)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "world.dat"), make([]byte, 3*chunkSize), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := &fakeCoreStream{}
	cc := &coreConnection{stream: stream}
	m := &MeshManager{handler: h}

	returned := make(chan struct{})
	go func() {
		m.handleRequest(cc, &pb.NodeMessage{
			RequestId: "dl", ServerUuid: uuid, FlowWindow: 1,
			Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: "world.dat"}},
		})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("handleRequest is still streaming the download on the read loop")
	}

	deadline := time.Now().Add(time.Second)
	for stream.chunks("dl") < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := stream.chunks("dl"); n != 1 {
		t.Fatalf("%d chunks went out on a window of one", n)
	}
	cc.grantFlow("dl", 2)
	deadline = time.Now().Add(time.Second)
	for stream.chunks("dl") < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := stream.chunks("dl"); n != 3 {
		t.Fatalf("after a grant of two, %d chunks in total, want 3", n)
	}
}

// The read loop hands a FlowCredit to the request it is for, and never to the
// request handler, which would answer it with "unknown request type".
func TestTheReadLoopTakesFlowCredit(t *testing.T) {
	cc := &coreConnection{}
	cc.openFlow("dl", 1)
	_ = cc.awaitCredit(chunkFor("dl"))
	if !cc.takeFlowCredit(&pb.NodeMessage{RequestId: "dl", Payload: &pb.NodeMessage_FlowCredit{FlowCredit: &pb.FlowCredit{Grant: 1}}}) {
		t.Fatal("a FlowCredit was not taken")
	}
	if cc.takeFlowCredit(chunkFor("dl")) {
		t.Fatal("a chunk was taken as credit")
	}
	done := make(chan error, 1)
	go func() { done <- cc.awaitCredit(chunkFor("dl")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the credit the read loop took did not reach the request")
	}
}

// Core grants credit only to a node that marks its counted messages, so every
// counted message of a controlled request must carry the mark - and nothing
// else, and no request without a window, may claim it.
func TestCountedMessagesOfAControlledRequestAreMarked(t *testing.T) {
	cc := &coreConnection{}
	cc.openFlow("dl", 4)
	chunk := chunkFor("dl")
	if err := cc.awaitCredit(chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.FlowWindow == 0 {
		t.Fatal("a controlled chunk went out unmarked: Core would never grant more")
	}
	done := &pb.NodeMessage{RequestId: "dl", Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{}}}
	free := chunkFor("no-window")
	_ = cc.awaitCredit(done)
	_ = cc.awaitCredit(free)
	if done.FlowWindow != 0 || free.FlowWindow != 0 {
		t.Fatal("a message outside flow control was marked")
	}
}

// Core's cancel ends the transfer at once, and the request stays held: a
// sender that went on would otherwise find no flow and send uncontrolled.
func TestCoresCancelEndsTheTransfer(t *testing.T) {
	cc := &coreConnection{}
	cc.openFlow("dl", 1)
	_ = cc.awaitCredit(chunkFor("dl"))
	waiting := make(chan error, 1)
	go func() { waiting <- cc.awaitCredit(chunkFor("dl")) }()
	time.Sleep(20 * time.Millisecond)
	if !cc.takeFlowCredit(&pb.NodeMessage{RequestId: "dl", Payload: &pb.NodeMessage_FlowCredit{FlowCredit: &pb.FlowCredit{Cancel: true}}}) {
		t.Fatal("a cancel was not taken")
	}
	select {
	case err := <-waiting:
		if err == nil {
			t.Fatal("a cancelled transfer sent on")
		}
	case <-time.After(time.Second):
		t.Fatal("the cancel did not release the waiting sender")
	}
	if err := cc.awaitCredit(chunkFor("dl")); err == nil {
		t.Fatal("a chunk after the cancel went out uncontrolled")
	}
}

// Downloads no longer queue behind the read loop, so the node bounds them
// itself - per server: one past the cap waits for a slot, while another
// server's download is not held by it.
func TestDownloadsBeyondTheCapWait(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	const busy, other = "44444444-4444-4444-4444-444444444444", "55555555-5555-5555-5555-555555555555"
	for _, uuid := range []string{busy, other} {
		root := sm.GetServerDir(uuid)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "a.dat"), make([]byte, 10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	slots := downloadSlotsFor(busy)
	for i := 0; i < downloadsPerServer; i++ {
		slots <- struct{}{} // the server's downloads are all running
	}
	stream := &fakeCoreStream{}
	cc := &coreConnection{stream: stream}
	m := &MeshManager{handler: NewStreamHandler(sm)}
	read := func(id, uuid string) {
		m.handleRequest(cc, &pb.NodeMessage{
			RequestId: id, ServerUuid: uuid,
			Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: "a.dat"}},
		})
	}
	waitChunk := func(id string) bool {
		deadline := time.Now().Add(time.Second)
		for stream.chunks(id) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		return stream.chunks(id) > 0
	}
	read("queued", busy)
	read("neighbour", other)
	if !waitChunk("neighbour") {
		t.Fatal("another server's download was held by this one's cap")
	}
	if stream.chunks("queued") != 0 {
		t.Fatal("a download past the cap started")
	}
	<-slots // one running download ends
	if !waitChunk("queued") {
		t.Fatal("the waiting download never started once a slot was free")
	}
	for i := 1; i < downloadsPerServer; i++ {
		<-slots
	}
}

// A transfer that gives up on a stalled reader tells Core so. The reader on
// Core has no deadline of its own and would otherwise wait for it forever.
func TestAStalledTransferTellsCore(t *testing.T) {
	prev := flowCreditWait
	flowCreditWait = 20 * time.Millisecond
	t.Cleanup(func() { flowCreditWait = prev })
	stream := &fakeCoreStream{}
	cc := &coreConnection{stream: stream}
	cc.openFlow("dl", 1)
	if err := cc.send(chunkFor("dl")); err != nil {
		t.Fatal(err)
	}
	if err := cc.send(chunkFor("dl")); !errors.Is(err, errFlowStalled) {
		t.Fatalf("err = %v, want the stall", err)
	}
	for _, msg := range stream.messages() {
		if msg.RequestId == "dl" && msg.GetError() != nil {
			return
		}
	}
	t.Fatal("Core was not told the transfer ended")
}
