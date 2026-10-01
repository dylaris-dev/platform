package backup

import (
	"context"
	"errors"
	"io"
	"testing"

	pb "dylaris-proto/node"
)

// A backup read from a node that stopped mid-archive ended in io.EOF, and the
// restore went ahead with the part it had.
func TestANodeLocalArchiveCutShortIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		final bool
		want  error
	}{
		{"whole archive", true, nil},
		{"cut short", false, io.ErrUnexpectedEOF},
	} {
		ch := make(chan *pb.NodeMessage, 3)
		ch <- &pb.NodeMessage{Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: []byte("abc")}}}
		if tc.final {
			ch <- &pb.NodeMessage{Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: 3}}}
		}
		close(ch)
		_, err := io.ReadAll(&nodeLocalReader{ctx: context.Background(), ch: ch})
		if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.want)
		}
	}
}
