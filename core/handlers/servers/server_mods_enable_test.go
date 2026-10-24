package servers

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"dylaris-core/handlers"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/store"

	pb "dylaris-proto/node"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"
)

// fileOpNode answers create/copy/delete and records them; failCopy refuses
// every copy.
type fileOpNode struct {
	grpc.ServerStream
	mu       sync.Mutex
	conn     *nodegrpc.NodeConnection
	ops      []string
	failCopy bool
}

func (s *fileOpNode) Context() context.Context       { return context.Background() }
func (s *fileOpNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *fileOpNode) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	var reply *pb.NodeMessage
	ok := &pb.NodeMessage{RequestId: m.RequestId, Payload: &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "ok"}}}
	switch {
	case m.GetCreateReq() != nil:
		s.ops = append(s.ops, "mkdir "+m.GetCreateReq().Path)
		reply = ok
	case m.GetCopyReq() != nil:
		s.ops = append(s.ops, "copy "+m.GetCopyReq().SrcPath+" -> "+m.GetCopyReq().DstPath)
		reply = ok
		if s.failCopy {
			reply = &pb.NodeMessage{RequestId: m.RequestId, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 500, Message: "disk full"}}}
		}
	case m.GetDeleteReq() != nil:
		s.ops = append(s.ops, "rm "+m.GetDeleteReq().Path)
		reply = ok
	}
	conn := s.conn
	s.mu.Unlock()
	if reply != nil {
		go conn.RouteResponse(reply)
	}
	return nil
}

type modEnableStore struct {
	store.Store
	srv      models.Server
	mod      models.ServerMod
	disabled []bool // every SetServerModDisabled call
}

func (f *modEnableStore) GetServerByID(int) (*models.Server, error) { s := f.srv; return &s, nil }
func (f *modEnableStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return []models.ServerMod{f.mod}, nil
}
func (f *modEnableStore) SetServerModDisabled(id, serverID int, d bool) error {
	f.disabled = append(f.disabled, d)
	f.mod.Disabled = d
	return nil
}

func TestSwitchingAModOffAndOnMovesItsJar(t *testing.T) {
	setup := func(failCopy bool) (*ServerModsHandler, *modEnableStore, *fileOpNode) {
		fs := &modEnableStore{
			srv: models.Server{ID: 5, UUID: "srv-5", NodeID: 7, ActiveSubServer: "main", InstallerType: "fabric"},
			mod: models.ServerMod{ID: 9, ServerID: 5, FileName: "lithium.jar", TargetDir: "mods", Status: models.ServerModInstalled},
		}
		reg := nodegrpc.NewRegistry()
		node := &fileOpNode{failCopy: failCopy}
		node.conn = reg.Register(7, "tok", node)
		return NewServerModsHandler(&handlers.AppState{Store: fs, GRPCRegistry: reg}), fs, node
	}
	put := func(h *ServerModsHandler, body string) int {
		req := httptest.NewRequest("PUT", "/api/servers/5/mods/9/enabled", strings.NewReader(body))
		req = mux.SetURLVars(req, map[string]string{"id": "5", "modId": "9"})
		rec := httptest.NewRecorder()
		h.SetEnabled(rec, req)
		return rec.Code
	}

	h, fs, node := setup(false)
	if code := put(h, `{"enabled":false}`); code != 200 {
		t.Fatalf("switch off answered %d", code)
	}
	want := []string{"mkdir main/mods/.disabled", "copy main/mods/lithium.jar -> main/mods/.disabled/lithium.jar", "rm main/mods/lithium.jar"}
	if strings.Join(node.ops, "|") != strings.Join(want, "|") {
		t.Fatalf("ops = %v, want %v", node.ops, want)
	}
	if len(fs.disabled) != 1 || !fs.disabled[0] {
		t.Fatalf("stored %v, want [true]", fs.disabled)
	}
	node.ops = nil
	if code := put(h, `{"enabled":true}`); code != 200 {
		t.Fatalf("switch on answered %d", code)
	}
	want = []string{"copy main/mods/.disabled/lithium.jar -> main/mods/lithium.jar", "rm main/mods/.disabled/lithium.jar"}
	if strings.Join(node.ops, "|") != strings.Join(want, "|") {
		t.Fatalf("ops = %v, want %v", node.ops, want)
	}

	// A failed copy deletes nothing and records nothing: the jar is still
	// where the row says it is.
	h, fs, node = setup(true)
	if code := put(h, `{"enabled":false}`); code != 502 {
		t.Fatalf("failed copy answered %d, want 502", code)
	}
	for _, op := range node.ops {
		if strings.HasPrefix(op, "rm ") {
			t.Fatalf("deleted after a failed copy: %v", node.ops)
		}
	}
	if len(fs.disabled) != 0 {
		t.Fatalf("stored %v after a failed move", fs.disabled)
	}
}
