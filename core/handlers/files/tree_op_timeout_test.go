package files

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"

	pb "dylaris-proto/node"
)

// slowNode takes every request and never answers in time, the way a node
// still walking a multi-GB folder copy or delete looks to Core.
type slowNode struct{ grpc.ServerStream }

func (slowNode) Context() context.Context       { return context.Background() }
func (slowNode) Recv() (*pb.NodeMessage, error) { select {} }
func (slowNode) Send(m *pb.NodeMessage) error   { return nil }

// A long copy or delete that outlived Core's wait was reported as a 502
// "Node communication error", although the node finishes it anyway: a retried
// copy then hit "already exists", and a "failed" delete had deleted.
func TestATreeOpCoreStopsWaitingForIsNotReportedAsFailed(t *testing.T) {
	prev := treeOpWait
	treeOpWait = 50 * time.Millisecond
	t.Cleanup(func() { treeOpWait = prev })
	for _, c := range []struct {
		name string
		url  string
		body string
		call func(*FileHandler, http.ResponseWriter, *http.Request)
	}{
		{"copy", "/api/files/copy?server_uuid=srv-1", `{"oldPath":"world","newPath":"world2"}`, (*FileHandler).CopyFileHandler},
		{"delete", "/api/files/delete?server_uuid=srv-1", `{"path":"world"}`, (*FileHandler).DeleteFileHandler},
	} {
		t.Run(c.name, func(t *testing.T) {
			reg := nodegrpc.NewRegistry()
			reg.Register(7, "tok", slowNode{})
			fs := writeFakeStore{}
			h := &FileHandler{d: Deps{Store: fs, Authz: authz.NewResolver(fs), GRPCRegistry: reg}}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.call(h, w, asOwner(httptest.NewRequest(http.MethodPost, c.url, strings.NewReader(c.body))))
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the handler did not use treeOpWait")
			}
			if w.Code != http.StatusGatewayTimeout {
				t.Errorf("status = %d, want 504", w.Code)
			}
			if !strings.Contains(w.Body.String(), "may still") {
				t.Errorf("body %q does not say the operation may still finish", w.Body)
			}
		})
	}
}
