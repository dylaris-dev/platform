package nodeops

import (
	"errors"
	"net/http"
	"testing"

	pb "dylaris-proto/node"
)

// partialStream is a node's answer cut off before its final TransferDone: the
// node went away, or the read stalled and was ended.
func partialStream(sizes ...int) chan *pb.NodeMessage {
	ch := make(chan *pb.NodeMessage, len(sizes)+1)
	for _, n := range sizes {
		ch <- &pb.NodeMessage{Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: make([]byte, n)}}}
	}
	return ch
}

// chunkStream is the whole answer: the chunks, then the final TransferDone.
func chunkStream(sizes ...int) <-chan *pb.NodeMessage {
	ch := partialStream(sizes...)
	ch <- &pb.NodeMessage{Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: 1}}}
	close(ch)
	return ch
}

// Opening a file collected all of it in Core's memory with no limit: one user
// with read access opening a multi-gigabyte file took Core down for everyone.
func TestOpeningAFileStopsAtTheLimit(t *testing.T) {
	if data, _, err := CollectNodeFile(chunkStream(4, 4), 10); err != nil || len(data) != 8 {
		t.Fatalf("a file under the limit: %d bytes, err %v", len(data), err)
	}
	if _, _, err := CollectNodeFile(chunkStream(6, 6), 10); !errors.Is(err, ErrFileTooLargeToOpen) {
		t.Fatalf("a file over the limit: err %v, want ErrFileTooLargeToOpen", err)
	}
}

// A node can be a customer's machine. Its 401 signed every visitor out; a code
// outside the HTTP range made WriteHeader panic.
func TestANodesErrorCodeIsNotPassedThroughBlindly(t *testing.T) {
	for code, want := range map[int32]int{
		404: http.StatusNotFound, 403: http.StatusForbidden, 413: http.StatusRequestEntityTooLarge,
		401: http.StatusBadGateway, 302: http.StatusBadGateway, 0: http.StatusBadGateway, 99999: http.StatusBadGateway,
	} {
		if got := NodeErrorStatus(code); got != want {
			t.Errorf("node code %d -> %d, want %d", code, got, want)
		}
	}
}

// A stream that closed without the final TransferDone was read as the whole
// file; an editor that saved it cut the file at that point.
func TestAFileCutShortIsNotOpenedAsTheWholeFile(t *testing.T) {
	ch := partialStream(4, 4)
	close(ch)
	if _, _, err := CollectNodeFile(ch, 100); !errors.Is(err, ErrTransferIncomplete) {
		t.Fatalf("err %v, want ErrTransferIncomplete", err)
	}
	if ReadErrStatus(ErrTransferIncomplete) != http.StatusBadGateway {
		t.Fatal("a cut-off read is not a size problem")
	}
}
