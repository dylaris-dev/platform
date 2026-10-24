package servers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	pb "dylaris-proto/node"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of handlers test fixtures this package's tests use; each keeps its name.

func foreignAdminRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", visAdmin)
	ctx = context.WithValue(ctx, "username", "admin")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

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

// foreignState builds the state the way main does: the resolver asks the same
// predicate the handlers ask.
func foreignState(node *models.Node) *handlers.AppState {
	st := &foreignFakeStore{node: node, byon: "true"}
	s := &handlers.AppState{Store: st, Authz: authz.NewResolver(st), FeatureFlags: services.NewFeatureFlags(st)}
	s.Authz.SetForeignNode(s.NodeOwnedByOther)
	return s
}

// funcBody returns the text of one top-level func, from its signature to the
// closing brace in column 0.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	re := regexp.MustCompile(`(?ms)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(fn) + `\(.*?^}`)
	body := re.FindString(string(src))
	if body == "" {
		t.Fatalf("%s not found in %s", fn, file)
	}
	return body
}

func newPlacementHandler(t *testing.T, nodes []models.Node) *handlers.PlacementHandler {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &placementFakeStore{nodes: nodes, settings: map[string]string{"routing_mode": "gateway"}}
	return handlers.NewPlacementHandler(&handlers.AppState{Store: fs, Redis: rdb})
}

func newQuotaHTTPRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func ownedNode(id int, name string, owner *string) models.Node {
	return models.Node{
		ID: id, Name: name, Token: name, Status: "online", OwnerID: owner,
		TotalRAMMB: 65536, RAMOvercommitRatio: 1.0, TotalCPU: 16, CPUOvercommitRatio: 1.0,
	}
}

func readVoiceFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "voice", name))
	if err != nil {
		t.Fatal(err)
	}
	// core.autocrlf may check the fixtures out with CRLF; the writers emit LF.
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// seedHeartbeat writes the discovery key the node writes, so the version comes
// from the same place the What's New view reads it from rather than from a
// second notion of "how old is this node".
func seedHeartbeat(t *testing.T, m *miniredis.Miniredis, token, version string) {
	t.Helper()
	hb := services.NodeHeartbeat{ID: token, ReleaseVersion: version}
	raw, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	m.Set("dylaris:discovery:"+token, string(raw))
}

const staffID = "33333333-3333-3333-3333-333333333333"

func staffRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", staffID)
	ctx = context.WithValue(ctx, "username", "staff")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// tabListAuthzStore is the resolver's store port, faked: grants per server id and
// per owner, nothing else. Every server is owned by "owner-1".
type tabListAuthzStore struct {
	grants  map[int]*store.ServerGrant
	account *store.ServerGrant
	failOn  int
}

func (f tabListAuthzStore) GetServerByID(id int) (*models.Server, error) {
	if id == f.failOn {
		return nil, errors.New("connection refused")
	}
	return &models.Server{ID: id, OwnerID: "owner-1"}, nil
}

func (f tabListAuthzStore) GetServerByUUID(string) (*models.Server, error) { return nil, sql.ErrNoRows }

func (f tabListAuthzStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, sql.ErrNoRows }

func (f tabListAuthzStore) GetServerRole(int) (*store.ServerRole, error) { return nil, sql.ErrNoRows }

func (f tabListAuthzStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f tabListAuthzStore) GetServerGrant(serverID int, _ string) (*store.ServerGrant, error) {
	if g, ok := f.grants[serverID]; ok {
		return g, nil
	}
	return nil, sql.ErrNoRows
}

func (f tabListAuthzStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	if f.account == nil {
		return nil, sql.ErrNoRows
	}
	return f.account, nil
}

func testConnectionProbeDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(posixTestBase(), testTmpRootName("dylaris-test-tmp"))
	dir := filepath.ToSlash(filepath.Join(root, t.Name()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return dir
}

type twoNodeFakeStore struct {
	foreignFakeStore
	target *models.Node
}

// An empty target: the per-node server cap does not refuse (TransferServer).
func (f *twoNodeFakeStore) CountServersByNode(int) (int, error) { return 0, nil }

func (f *twoNodeFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.target != nil && id == f.target.ID {
		return f.target, nil
	}
	return f.node, nil
}

func (f *twoNodeFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", OwnerID: visOwner, NodeID: f.node.ID}, nil
}

// The transfer asks whether the owner's account is cut off; it is not here.
func (f *twoNodeFakeStore) GetUserBilling(id string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: id, Status: "active"}, nil
}

const (
	visOwner = "11111111-1111-1111-1111-111111111111"
	visAdmin = "22222222-2222-2222-2222-222222222222"
)

func visOwnedNode() *models.Node {
	owner := visOwner
	return &models.Node{ID: 7, Status: "online", OwnerID: &owner}
}

func visPlatformNode() *models.Node {
	return &models.Node{ID: 7, Status: "online"}
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

func voiceRaceState(t *testing.T, fs *voiceRaceStore) (*handlers.AppState, *miniredis.Miniredis) {
	t.Helper()
	st, mr, _ := voiceNodeState(t, fs, "config/voicechat/voicechat-server.properties", "svc_before.properties")
	return st, mr
}

type voiceRaceStore struct {
	store.Store
	row     store.VoicePort  // what the sync reads
	current *store.VoicePort // what the table holds by the time it writes; nil = deleted
	updates int
	allocs  int
	mod     string
}

func (f *voiceRaceStore) GetVoicePort(int) (*store.VoicePort, error) {
	r := f.row
	return &r, nil
}

func (f *voiceRaceStore) UpdateVoicePortMod(serverID int, region string, port int, mod string, internalPort int, host string) (bool, error) {
	f.updates++
	f.mod = mod
	c := f.current
	return c != nil && c.ServerID == serverID && c.Region == region && c.Port == port, nil
}

func (f *voiceRaceStore) AllocateVoicePort(store.VoicePort, int, int) (store.VoicePort, error) {
	f.allocs++
	return store.VoicePort{}, nil
}

type voiceSyncFakeStore struct {
	store.Store
	row   *store.VoicePort
	calls int
}

func (f *voiceSyncFakeStore) GetVoicePort(int) (*store.VoicePort, error) {
	f.calls++
	return f.row, nil
}

type placementFakeStore struct {
	store.Store
	nodes    []models.Node
	settings map[string]string
}

func (f *placementFakeStore) ListNodes() ([]models.Node, error) { return f.nodes, nil }

func (f *placementFakeStore) GetSetting(k string) (string, error) {
	return f.settings[k], nil
}

func (f *placementFakeStore) SumAllocatedByNode(int) (int64, float64, error) { return 0, 0, nil }

func (f *placementFakeStore) CountServersByNode(int) (int, error) { return 0, nil }

// testConnectionProbeDir returns an absolute-looking POSIX path ("/...")
// that os.MkdirAll can still create on a Windows dev box (Windows treats a
// leading "/" as drive-root-relative), matching validateCoreStorageConfig's
// deliberately Linux-only absolute-path check (see its doc comment). Same
// convention as the "/mnt/shared" literal already used in
// core_storage_builder_test.go; a real t.TempDir() would fail the "/" check
// on Windows and was deliberately not used here for that reason.
// posixTestBase is where the "/"-prefixed test directories live: the drive
// root on a Windows dev box (see testConnectionProbeDir), the OS temp dir
// everywhere else. Not "/" on Linux: CI runs on GitHub-hosted runners, which
// are not root, so nothing can be created at the filesystem root there.
func posixTestBase() string {
	if runtime.GOOS == "windows" {
		return "/"
	}
	return os.TempDir()
}

// testTmpRootName makes the shared test root per test binary. Every helper
// above removes its whole root on cleanup, and since handlers was split the
// packages that use one run in parallel; a shared name let one package's
// cleanup delete another's directories mid-test.
func testTmpRootName(base string) string { return fmt.Sprintf("%s-%d", base, os.Getpid()) }

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

type demoListStore struct {
	store.Store
	settings map[string]string
	// deleteInLock deletes the server between the handler's first load and
	// the locked edit, as a delete on the other replica would.
	deleteInLock bool
	deleted      bool
}

func (f *demoListStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func (f *demoListStore) SetSetting(k, v string) error { f.settings[k] = v; return nil }

func (f *demoListStore) UpdateSetting(k string, fn func(string) (string, error)) error {
	if f.deleteInLock {
		f.deleted = true
	}
	v, err := fn(f.settings[k])
	if err == nil {
		f.settings[k] = v
	}
	return err
}

func (f *demoListStore) GetServerByID(id int) (*models.Server, error) {
	if f.deleted {
		return nil, sql.ErrNoRows
	}
	return &models.Server{ID: id, UUID: "srv-" + strconv.Itoa(id), NodeID: 1}, nil
}

func (f *demoListStore) GetNodeByID(id int) (*models.Node, error) { return &models.Node{ID: id}, nil }

type demoStaffStore struct {
	store.Store
	user  *models.User
	roleC []string
}

func (f *demoStaffStore) GetUserByUsername(string) (*models.User, error) { return f.user, nil }

func (f *demoStaffStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	if f.roleC == nil {
		return nil, store.CapOverrides{}, nil
	}
	id := 1
	return &id, store.CapOverrides{}, nil
}

func (f *demoStaffStore) GetPanelRole(int) (*store.PanelRole, error) {
	return &store.PanelRole{Capabilities: f.roleC}, nil
}

func (f *demoStaffStore) SetSetting(string, string) error { return nil }

// memberDeleteStore gives the caller members.delete on server 5 through a role.
type memberDeleteStore struct{ *revokeStore }

func (f *memberDeleteStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	g, err := f.revokeStore.GetServerGrant(serverID, userID)
	if g != nil && serverID == 5 {
		one := 1
		g.ServerRoleID = &one
	}
	return g, err
}

func (f *memberDeleteStore) GetServerRole(int) (*store.ServerRole, error) {
	return &store.ServerRole{ID: 1, Capabilities: []string{"members.delete"}}, nil
}

// revokeStore: backend 5 linked to proxy 9, both owned by "owner"; "member"
// holds a narrow direct grant on 5 and an inherit grant on 9.
type revokeStore struct {
	store.Store
	deleted    bool
	proxyGrant bool
	staleRealm bool
}

func (f *revokeStore) GetServerByID(id int) (*models.Server, error) {
	if id == 9 {
		return &models.Server{ID: 9, OwnerID: "owner", ServerType: "proxy"}, nil
	}
	p := 9
	return &models.Server{ID: 5, OwnerID: "owner", ProxyID: &p}, nil
}

func (f *revokeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if serverID == 9 && f.proxyGrant {
		realm := "owner"
		if f.staleRealm {
			realm = "previous-owner"
		}
		return &store.ServerGrant{UserID: userID, OwnerUserID: realm, Inherit: true}, nil
	}
	return &store.ServerGrant{UserID: userID, OwnerUserID: "owner"}, nil
}

func (f *revokeStore) GetUserByUsername(name string) (*models.User, error) {
	return &models.User{ID: "target", Username: name}, nil
}

func (f *revokeStore) GetNodeByID(id int) (*models.Node, error) { return &models.Node{ID: id}, nil }

func (f *revokeStore) DeleteInvite(int, string) error { f.deleted = true; return nil }

func (f *revokeStore) DeleteServerGrant(*int, string, string) error {
	f.deleted = true
	return nil
}

func (f *revokeStore) GetSetting(string) (string, error) { return "", nil }

func (f *revokeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, nil
}

func (f *revokeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *revokeStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }

func (f *revokeStore) GetServerAuditState(int) (bool, bool, int, error) { return false, false, 0, nil }
