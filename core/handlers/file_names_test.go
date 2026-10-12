package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"

	pb "dylaris-proto/node"
)

// A file name is the tenant's: "New World" and "fabric-api+1.20.1.jar" are
// what Minecraft and mod authors write. Core used to rewrite every character
// outside [A-Za-z0-9._+-] to '_' without saying so. It now passes a plain name
// through and refuses one that is a path. The suspended tenant makes a name
// that got past the check visible as the suspension refusal, before any node.
func TestCreateAndRenameCheckTheNameInsteadOfRewritingIt(t *testing.T) {
	rdb, _ := newQuotaHTTPRedis(t)
	st := suspendedUploadStore{quotaHTTPFakeStore{newCoreStorageHTTPFakeStore()}}
	h := &FileHandler{state: &AppState{Redis: rdb, Store: st, Authz: authz.NewResolver(st)}}

	call := func(fn func(http.ResponseWriter, *http.Request), body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/files/x?server_uuid=s1", strings.NewReader(body))
		ctx := context.WithValue(req.Context(), "username", "u")
		ctx = context.WithValue(ctx, "isAdmin", true)
		ctx = context.WithValue(ctx, apiKeyCtxKey{}, &apiKeyCtx{key: &models.APIKey{ID: 1}})
		rw := httptest.NewRecorder()
		fn(rw, req.WithContext(ctx))
		return rw
	}
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"New World", true},
		{"fabric-api-0.92.0+1.20.1.jar", true},
		{"Übersetzung.txt", true},
		{"..", false},
		{"", false},
		{`a\b`, false},
	} {
		p, _ := json.Marshal("world/" + tc.name)
		create := call(h.CreateFileHandler, `{"path":`+string(p)+`}`)
		rename := call(h.RenameFileHandler, `{"oldPath":"a.txt","newPath":`+string(p)+`}`)
		for op, rw := range map[string]*httptest.ResponseRecorder{"create": create, "rename": rename} {
			if tc.ok && rw.Code != http.StatusForbidden {
				t.Errorf("%s %q: status %d (%s), want it to reach the suspension check", op, tc.name, rw.Code, rw.Body)
			}
			if !tc.ok && rw.Code != http.StatusBadRequest {
				t.Errorf("%s %q: status %d (%s), want 400", op, tc.name, rw.Code, rw.Body)
			}
		}
	}
}

// listNode answers every ListReq with a fixed reply.
type listNode struct {
	grpc.ServerStream
	mu    sync.Mutex
	conn  *nodegrpc.NodeConnection
	reply func(id string) *pb.NodeMessage
}

func (s *listNode) Context() context.Context       { return context.Background() }
func (s *listNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *listNode) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	go conn.RouteResponse(s.reply(m.RequestId))
	return nil
}

// Nodes from 2026.10.11.7 answer a missing folder with a 404 instead of an
// empty list. The internal listings (mods/ and plugins/ on a vanilla server)
// must still read that as "nothing there", or a vanilla server's unmanaged
// jars listing becomes a 502. Any other error stays an error.
func TestAMissingFolderListsAsEmptyForCore(t *testing.T) {
	for _, tc := range []struct {
		code    int32
		wantErr bool
	}{{404, false}, {403, true}, {500, true}} {
		reg := nodegrpc.NewRegistry()
		node := &listNode{reply: func(id string) *pb.NodeMessage {
			return &pb.NodeMessage{RequestId: id, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: tc.code, Message: "x"}}}
		}}
		conn := reg.Register(7, "tok", node)
		node.mu.Lock()
		node.conn = conn
		node.mu.Unlock()

		files, err := listServerDirFor(&AppState{GRPCRegistry: reg}, 7, "srv-1", "survival/mods")
		if (err != nil) != tc.wantErr {
			t.Errorf("node code %d: err = %v, want error %v", tc.code, err, tc.wantErr)
		}
		if !tc.wantErr && (files == nil || len(files) != 0) {
			t.Errorf("node code %d: files = %v, want an empty list", tc.code, files)
		}
	}
}

// What a node before 2026.10.11.7 would strip from a rename's new name. Those
// renames are refused to such a node; the rest still reach it.
func TestOldNodeWouldRewrite(t *testing.T) {
	for name, want := range map[string]bool{
		"server.jar":            false,
		"fabric-api+1.20.1.jar": false,
		"New World":             true,
		"Übersetzung.txt":       true,
		"a&b.txt":               true,
	} {
		if got := oldNodeWouldRewrite(name); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
}

// renameGateStore is writeFakeStore plus the node row the version gate reads.
type renameGateStore struct{ writeFakeStore }

func (renameGateStore) GetNodeByID(int) (*models.Node, error) {
	return &models.Node{ID: 7, Token: "tok"}, nil
}

// A node of unknown version (no heartbeat here) is treated as old, so a name
// it would rewrite is refused before the node sees the rename.
func TestARenameAnOldNodeWouldRewriteIsRefused(t *testing.T) {
	st := writeState(t, false)
	st.Store = renameGateStore{}
	h := &FileHandler{state: st}
	r := httptest.NewRequest(http.MethodPost, "/api/files/rename?server_uuid=srv-1",
		strings.NewReader(`{"oldPath":"a.txt","newPath":"New World"}`))
	w := httptest.NewRecorder()
	h.RenameFileHandler(w, asOwner(r))
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d (%s), want 409", w.Code, w.Body)
	}
}
