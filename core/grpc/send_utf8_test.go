package nodegrpc

import (
	"testing"

	pb "dylaris-proto/node"

	"google.golang.org/protobuf/proto"
)

// proto3 refuses to marshal a string that is not UTF-8, and grpc-go ends the
// node's whole stream on that error: GET /api/files?path=%FF or a tab request
// header with an obs-text byte cut every transfer, console and tab on the node.
func TestSendRepairsInvalidUTF8(t *testing.T) {
	cases := []struct {
		name string
		msg  *pb.NodeMessage
		got  func(*pb.NodeMessage) string
	}{
		{"file path from a query string", &pb.NodeMessage{RequestId: "r1", Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: "a\xffb"}}},
			func(m *pb.NodeMessage) string { return m.GetListReq().GetPath() }},
		{"tab request header", &pb.NodeMessage{RequestId: "r2", Payload: &pb.NodeMessage_HttpProxyReq{HttpProxyReq: &pb.HttpProxyReq{Headers: []*pb.HttpHeader{{Key: "X-A", Value: "v\x81z"}}}}},
			func(m *pb.NodeMessage) string { return m.GetHttpProxyReq().GetHeaders()[0].GetValue() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &lockedStream{}
			conn := &NodeConnection{Stream: s}
			if err := conn.Send(c.msg); err != nil {
				t.Fatal(err)
			}
			out := s.sent[0]
			if _, err := proto.Marshal(out); err != nil {
				t.Fatalf("still does not marshal: %v", err)
			}
			if c.got(out) == "" {
				t.Fatal("the field was emptied instead of repaired")
			}
		})
	}
}
