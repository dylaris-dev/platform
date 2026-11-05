package nodegrpc

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "dylaris-proto/node"
)

// listNode answers the one request it is sent with replies, in order.
type listNode struct {
	grpc.ServerStream
	mu      sync.Mutex
	conn    *NodeConnection
	replies []*pb.NodeMessage
	got     *pb.NodeMessage
}

func (s *listNode) Context() context.Context       { return context.Background() }
func (s *listNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *listNode) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	s.got = m
	conn := s.conn
	s.mu.Unlock()
	go func() {
		for _, r := range s.replies {
			r.RequestId = m.RequestId
			conn.RouteResponse(r)
		}
	}()
	return nil
}

func listPart(more bool, names ...string) *pb.NodeMessage {
	files := make([]*pb.FileInfo, 0, len(names))
	for _, n := range names {
		files = append(files, &pb.FileInfo{Name: n})
	}
	return &pb.NodeMessage{Payload: &pb.NodeMessage_ListResp{ListResp: &pb.ListFilesResp{Files: files, More: more}}}
}

// A folder of thousands of entries did not fit one message, so the node could
// only answer 502. Core now asks for the listing in parts and joins them; a
// node that does not page answers in one, which is the whole listing.
func TestSendRequestJoinsAPagedListing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		replies   []*pb.NodeMessage
		wantNames []string
		wantCode  int32
	}{
		{"parts", []*pb.NodeMessage{listPart(true, "a", "b"), listPart(true, "c"), listPart(false, "d")}, []string{"a", "b", "c", "d"}, 0},
		{"a node that does not page", []*pb.NodeMessage{listPart(false, "a")}, []string{"a"}, 0},
		{"an error ends it", []*pb.NodeMessage{listPart(true, "a"), {Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 500}}}}, nil, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			node := &listNode{replies: tc.replies}
			conn := reg.Register(7, "tok", node)
			node.mu.Lock()
			node.conn = conn
			node.mu.Unlock()

			resp, err := reg.SendRequest(7, &pb.NodeMessage{RequestId: "ls", Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: "/"}}}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			node.mu.Lock()
			paged := node.got.GetListReq().GetPaged()
			node.mu.Unlock()
			if !paged {
				t.Error("the request did not ask for a paged listing")
			}
			if got := resp.GetError().GetCode(); got != tc.wantCode {
				t.Fatalf("error code %d, want %d", got, tc.wantCode)
			}
			if tc.wantCode != 0 {
				return
			}
			var names []string
			for _, f := range resp.GetListResp().GetFiles() {
				names = append(names, f.Name)
			}
			if len(names) != len(tc.wantNames) {
				t.Fatalf("names %v, want %v", names, tc.wantNames)
			}
			for i := range names {
				if names[i] != tc.wantNames[i] {
					t.Fatalf("names %v, want %v", names, tc.wantNames)
				}
			}
			if resp.GetListResp().More {
				t.Error("the joined listing still says more")
			}
		})
	}
}

// A node that stops after a part leaves the listing incomplete: an error, not
// a short listing taken for the whole folder.
func TestSendRequestRefusesAnUnfinishedListing(t *testing.T) {
	reg := NewRegistry()
	node := &listNode{replies: []*pb.NodeMessage{listPart(true, "a")}}
	conn := reg.Register(7, "tok", node)
	node.mu.Lock()
	node.conn = conn
	node.mu.Unlock()
	if _, err := reg.SendRequest(7, &pb.NodeMessage{RequestId: "ls", Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{}}}, 50*time.Millisecond); err == nil {
		t.Fatal("half a listing was returned as the whole")
	}
}
