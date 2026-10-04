package handlers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

// writeNode is a node that accepts a write and then answers its TransferDone
// the way the real one does after committing the file: a result, or the
// refusal (413 on a full disk quota).
type writeNode struct {
	grpc.ServerStream
	conn   *nodegrpc.NodeConnection
	refuse bool
	mu     sync.Mutex
}

func (s *writeNode) Context() context.Context { return context.Background() }
func (s *writeNode) Recv() (*pb.NodeMessage, error) {
	select {}
}
func (s *writeNode) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	var reply *pb.NodeMessage
	switch {
	case m.GetWriteReq() != nil:
		reply = &pb.NodeMessage{RequestId: m.RequestId, Payload: &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "ready"}}}
	case m.GetTransferDone() != nil && s.refuse:
		reply = &pb.NodeMessage{RequestId: m.RequestId, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 413, Message: "Storage limit reached"}}}
	case m.GetTransferDone() != nil:
		reply = &pb.NodeMessage{RequestId: m.RequestId, Payload: &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "written"}}}
	}
	if reply != nil {
		go conn.RouteResponse(reply)
	}
	return nil
}

// writeFakeStore owns one server on node 7.
type writeFakeStore struct{ store.Store }

func (writeFakeStore) GetServerByUUID(string) (*models.Server, error) {
	return &models.Server{ID: 1, UUID: "srv-1", OwnerID: "u1", NodeID: 7}, nil
}
func (writeFakeStore) GetServerByID(int) (*models.Server, error) {
	return &models.Server{ID: 1, UUID: "srv-1", OwnerID: "u1", NodeID: 7}, nil
}
func (writeFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (writeFakeStore) GetSetting(string) (string, error) { return "", nil }
func (writeFakeStore) GetUserBilling(id string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: id, Status: "active"}, nil
}

func writeState(t *testing.T, refuse bool) *AppState {
	t.Helper()
	reg := nodegrpc.NewRegistry()
	node := &writeNode{refuse: refuse}
	conn := reg.Register(7, "tok", node)
	node.mu.Lock()
	node.conn = conn
	node.mu.Unlock()
	fs := writeFakeStore{}
	return &AppState{Store: fs, Authz: authz.NewResolver(fs), GRPCRegistry: reg}
}

func asOwner(r *http.Request) *http.Request {
	ctx := context.WithValue(r.Context(), "userID", "u1")
	ctx = context.WithValue(ctx, "username", "alice")
	return r.WithContext(context.WithValue(ctx, "isAdmin", false))
}

// The node commits a written file on TransferDone and says how that went.
// Nobody listened: a save over a full disk quota was reported saved, and the
// user found out when the file was not there.
func TestAWriteTheNodeRefusedIsNotReportedSaved(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		h := &FileHandler{state: writeState(t, refuse)}
		r := httptest.NewRequest(http.MethodPost, "/api/files/save?server_uuid=srv-1",
			strings.NewReader(`{"path":"server.properties","content":"motd=hi"}`))
		w := httptest.NewRecorder()
		h.SaveFileHandler(w, asOwner(r))
		if refuse && w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("a refused save answered %d: %s", w.Code, w.Body)
		}
		if !refuse && w.Code != http.StatusOK {
			t.Fatalf("a committed save answered %d: %s", w.Code, w.Body)
		}
	}
}

func TestAnUploadTheNodeRefusedIsNotReportedUploaded(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		h := &FileHandler{state: writeState(t, refuse)}
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		_ = mw.WriteField("server_uuid", "srv-1")
		_ = mw.WriteField("path", "plugins")
		fw, _ := mw.CreateFormFile("files", "a.jar")
		_, _ = fw.Write([]byte("jar-bytes"))
		_ = mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/api/files/upload?server_uuid=srv-1", body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		h.UploadFileHandler(w, asOwner(r))
		if refuse && w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("a refused upload answered %d: %s", w.Code, w.Body)
		}
		if !refuse && w.Code != http.StatusOK {
			t.Fatalf("a committed upload answered %d: %s", w.Code, w.Body)
		}
	}
}

func TestAPropertiesWriteTheNodeRefusedIsAnError(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		h := &RconHandler{state: writeState(t, refuse)}
		err := h.writeNodeFileString(7, "srv-1", "server.properties", "motd=hi")
		if (err != nil) != refuse {
			t.Fatalf("refuse=%v: err %v", refuse, err)
		}
	}
}
