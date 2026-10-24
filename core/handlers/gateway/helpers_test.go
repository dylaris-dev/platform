package gateway

import (
	"bytes"
	"context"
	pb "dylaris-proto/node"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of handlers test fixtures the gateway tests use. Test files cannot be
// imported across packages; each keeps its original name so the source is easy
// to find with a search.

// foreignFakeStore answers the node, the server on it and the BYON flag. A
// settingErr or nodeErr is a database fault, which must count as foreign.
type foreignFakeStore struct {
	store.Store
	node       *models.Node
	nodeErr    error
	byon       string
	settingErr error
}

func (f *foreignFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	return f.node, nil
}

func (f *foreignFakeStore) GetSetting(key string) (string, error) {
	if key == "feature_byon_enabled" {
		return f.byon, f.settingErr
	}
	return "", nil
}

func (f *foreignFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", OwnerID: visOwner, NodeID: f.node.ID}, nil
}

func (f *foreignFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	return &models.Server{ID: 3, UUID: uuid, OwnerID: visOwner, NodeID: f.node.ID}, nil
}

func (f *foreignFakeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id}, nil
}

func (f *foreignFakeStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

func (f *foreignFakeStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }

func (f *foreignFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *foreignFakeStore) GetServerGrant(int, string) (*store.ServerGrant, error) {
	return nil, errors.New("no grant")
}

func (f *foreignFakeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, errors.New("no grant")
}

func foreignAdminRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", visAdmin)
	ctx = context.WithValue(ctx, "username", "admin")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

func visPlatformNode() *models.Node {
	return &models.Node{ID: 7, Status: "online"}
}

const (
	visOwner = "11111111-1111-1111-1111-111111111111"
	visAdmin = "22222222-2222-2222-2222-222222222222"
)

func newQuotaHTTPRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func decodeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out.Code
}

// newEntGateState builds a BYON-on state. storeEnabled is what separates a
// hosted install (entitlement required) from a self-host one (everything
// allowed), and getting it wrong in either direction is the whole risk here.
func newEntGateState(billing *store.UserBilling, storeEnabled bool) *handlers.AppState {
	fs := &entGateStore{
		settings: map[string]string{"feature_byon_enabled": "true"},
		billing:  billing,
	}
	return &handlers.AppState{
		Store:        fs,
		FeatureFlags: services.NewFeatureFlags(fs),
		StoreEnabled: storeEnabled,
		// Deliberately blank: an unreachable storefront must not turn the refusal
		// into a "you are not connected" claim we cannot support. See
		// storeLinkedBestEffort.
		StoreURL: "",
	}
}

// entGateStore serves the two lookups the gate makes.
type entGateStore struct {
	store.Store
	settings map[string]string
	billing  *store.UserBilling
}

func (f *entGateStore) GetSetting(key string) (string, error) { return f.settings[key], nil }

func (f *entGateStore) GetUserBilling(string) (*store.UserBilling, error) {
	return f.billing, nil
}

func (f *entGateStore) CountNodesByOwner(string) (int, error) { return 0, nil }

func (f *entGateStore) CountPendingNodeEnrollTokens(string) (int, error) { return 0, nil }

// Both pending kinds, because services.NodeSlotsUsed asks for both: a tenant can
// reach a node through an enroll token OR a warp key, and a fake that answers
// only the one this file is about would let the mint gate be tested against a
// number the production gate never sees.
func (f *entGateStore) CountNodeWarpKeysByOwner(string) (int, error) { return 0, nil }

func (f *entGateStore) CreateNodeEnrollToken(string, string, string, *time.Time, string) error {
	return nil
}

func ptrI64(i int64) *int64 { return &i }

func readVoiceFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "voice", name))
	if err != nil {
		t.Fatal(err)
	}
	// core.autocrlf may check the fixtures out with CRLF; the writers emit LF.
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// voiceNode serves one voice config at path and records what is written, so a
// sync gets past the node and reaches the store and Redis.
type voiceNode struct {
	grpc.ServerStream
	mu      sync.Mutex
	conn    *nodegrpc.NodeConnection
	path    string
	also    string // a second path served with the same file
	file    string
	written strings.Builder
	// failWrite answers every write request with an error.
	failWrite bool
	writes    int // write requests seen
}

func (s *voiceNode) Context() context.Context { return context.Background() }

func (s *voiceNode) Recv() (*pb.NodeMessage, error) { select {} }

func (s *voiceNode) Send(m *pb.NodeMessage) error {
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
	case m.GetReadReq() != nil && (strings.HasSuffix(m.GetReadReq().Path, "/"+s.path) || (s.also != "" && strings.HasSuffix(m.GetReadReq().Path, "/"+s.also))):
		replies = []*pb.NodeMessage{
			{RequestId: id, Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: []byte(s.file)}}},
			{RequestId: id, Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: int64(len(s.file))}}},
		}
	case m.GetReadReq() != nil:
		replies = []*pb.NodeMessage{{RequestId: id, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 404, Message: "not found"}}}}
	case m.GetWriteReq() != nil && s.countWrite() && s.failWrite:
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

func (s *voiceNode) countWrite() bool {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return true
}

func voiceNodeState(t *testing.T, fs store.Store, path, fixture string) (*handlers.AppState, *miniredis.Miniredis, *voiceNode) {
	t.Helper()
	reg := nodegrpc.NewRegistry()
	node := &voiceNode{path: path, file: readVoiceFixture(t, fixture)}
	conn := reg.Register(7, "tok", node)
	node.mu.Lock()
	node.conn = conn
	node.mu.Unlock()
	mr := miniredis.RunT(t)
	return &handlers.AppState{Store: fs, GRPCRegistry: reg, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}, mr, node
}

type flagSettings map[string]string

func (f flagSettings) GetSetting(key string) (string, error) { return f[key], nil }
