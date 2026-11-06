package files

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"

	pb "dylaris-proto/node"
)

// readNode answers a download with a scripted stream, the way the real node
// reports a failure mid-transfer: an Error, never a final TransferDone.
type readNode struct {
	grpc.ServerStream
	conn    *nodegrpc.NodeConnection
	replies []*pb.NodeMessage
}

func (s *readNode) Context() context.Context       { return context.Background() }
func (s *readNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *readNode) Send(m *pb.NodeMessage) error {
	if m.GetReadReq() == nil && m.GetSelectiveReadReq() == nil {
		return nil
	}
	go func() {
		for _, r := range s.replies {
			r.RequestId = m.RequestId
			s.conn.RouteResponse(r)
		}
	}()
	return nil
}

// A node error in the middle of a download used to end the response cleanly:
// a truncated zip (or, before the first chunk, an EMPTY attachment) was saved
// by the browser as a finished download.
func TestADownloadTheNodeFailsIsNotSavedAsComplete(t *testing.T) {
	meta := func() *pb.NodeMessage {
		return &pb.NodeMessage{Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{Filename: "world.zip"}}}
	}
	chunk := func() *pb.NodeMessage {
		return &pb.NodeMessage{Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: []byte("PK\x03\x04partial")}}}
	}
	fail := func() *pb.NodeMessage {
		return &pb.NodeMessage{Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 500, Message: "zip walk failed"}}}
	}
	handlers := []struct {
		name string
		url  string
		call func(*FileHandler, http.ResponseWriter, *http.Request)
	}{
		{"download", "/api/files/download?server_uuid=srv-1&path=world", (*FileHandler).DownloadFileHandler},
		{"selective", "/api/files/download/selective?server_uuid=srv-1&base_path=world&selected=level.dat", (*FileHandler).SelectiveDownloadHandler},
	}
	cases := []struct {
		name      string
		replies   func() []*pb.NodeMessage
		wantAbort bool
	}{
		{"error after the metadata frame", func() []*pb.NodeMessage { return []*pb.NodeMessage{meta(), fail()} }, false},
		{"error after body bytes", func() []*pb.NodeMessage { return []*pb.NodeMessage{meta(), chunk(), fail()} }, true},
	}
	for _, hc := range handlers {
		for _, c := range cases {
			t.Run(hc.name+"/"+c.name, func(t *testing.T) {
				node := &readNode{replies: c.replies()}
				reg := nodegrpc.NewRegistry()
				node.conn = reg.Register(7, "tok", node)
				fs := writeFakeStore{}
				h := &FileHandler{d: Deps{Store: fs, Authz: authz.NewResolver(fs), GRPCRegistry: reg}}

				w := httptest.NewRecorder()
				aborted := func() (aborted bool) {
					defer func() {
						if r := recover(); r != nil {
							if r != http.ErrAbortHandler {
								panic(r)
							}
							aborted = true
						}
					}()
					hc.call(h, w, asOwner(httptest.NewRequest(http.MethodGet, hc.url, nil)))
					return false
				}()

				if aborted != c.wantAbort {
					t.Fatalf("aborted = %v, want %v (status %d, body %q)", aborted, c.wantAbort, w.Code, w.Body)
				}
				if !c.wantAbort {
					if w.Code != http.StatusInternalServerError {
						t.Errorf("status = %d, want the node's 500", w.Code)
					}
					if cd := w.Header().Get("Content-Disposition"); cd != "" {
						t.Errorf("Content-Disposition survived: %q, the browser would save the error as world.zip", cd)
					}
				}
			})
		}
	}
}
