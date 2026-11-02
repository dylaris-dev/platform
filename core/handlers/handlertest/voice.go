package handlertest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	pb "dylaris-proto/node"

	nodegrpc "dylaris-core/grpc"
)

// VoiceFixture reads handlers/testdata/voice/<name> with line endings
// normalised to LF: core.autocrlf may check the fixtures out with CRLF, and
// the writers emit LF.
func VoiceFixture(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(HandlersRoot(t), "testdata", "voice", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// VoiceNode serves one voice config at Path and records what is written, so a
// sync gets past the node and reaches the store and Redis.
type VoiceNode struct {
	grpc.ServerStream
	mu      sync.Mutex
	conn    *nodegrpc.NodeConnection
	Path    string
	Also    string // a second path served with the same file
	File    string
	written strings.Builder
	// FailWrite answers every write request with an error.
	FailWrite bool
	writes    int // write requests seen
}

// WrittenString is everything written to the node so far.
func (s *VoiceNode) WrittenString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written.String()
}

// WriteCount is the number of write requests the node has seen.
func (s *VoiceNode) WriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// NewVoiceNode registers a VoiceNode serving the fixture at path as node 7,
// token "tok", in reg.
func NewVoiceNode(t testing.TB, reg *nodegrpc.Registry, path, fixture string) *VoiceNode {
	t.Helper()
	node := &VoiceNode{Path: path, File: VoiceFixture(t, fixture)}
	conn := reg.Register(7, "tok", node)
	node.mu.Lock()
	node.conn = conn
	node.mu.Unlock()
	return node
}

func (s *VoiceNode) Context() context.Context { return context.Background() }

func (s *VoiceNode) Recv() (*pb.NodeMessage, error) { select {} }

func (s *VoiceNode) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	id := m.RequestId
	var replies []*pb.NodeMessage
	switch {
	case m.GetChunk() != nil:
		s.mu.Lock()
		s.written.Write(m.GetChunk().Data)
		s.mu.Unlock()
	case m.GetReadReq() != nil && (strings.HasSuffix(m.GetReadReq().Path, "/"+s.Path) || (s.Also != "" && strings.HasSuffix(m.GetReadReq().Path, "/"+s.Also))):
		replies = []*pb.NodeMessage{
			{RequestId: id, Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: []byte(s.File)}}},
			{RequestId: id, Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: int64(len(s.File))}}},
		}
	case m.GetReadReq() != nil:
		replies = []*pb.NodeMessage{{RequestId: id, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 404, Message: "not found"}}}}
	case m.GetWriteReq() != nil && s.countWrite() && s.FailWrite:
		replies = []*pb.NodeMessage{{RequestId: id, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 500, Message: "disk full"}}}}
	case m.GetWriteReq() != nil, m.GetTransferDone() != nil:
		replies = []*pb.NodeMessage{{RequestId: id, Payload: &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "ok"}}}}
	}
	go func() {
		for _, r := range replies {
			conn.RouteResponse(r)
		}
		// The server's read loop closes a finished read stream; do the same.
		if m.GetReadReq() != nil {
			conn.CloseStreamingRequest(id)
		}
	}()
	return nil
}

func (s *VoiceNode) countWrite() bool {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return true
}
