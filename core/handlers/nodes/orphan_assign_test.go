package nodes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

// orphanAssignFakeStore embeds store.Store (nil) so it satisfies the interface
// at compile time. Only the node lookup and GetServerByUUID are reached: every
// case below is decided by the node check or the duplicate check, before any
// user or server is created. node_visibility_test.go reuses it.
type orphanAssignFakeStore struct {
	store.Store
	server *models.Server
	err    error
	// node is what GetNodeByID answers; nil means a plain platform node, which
	// is what the duplicate-check cases below need to get past the node check.
	node *models.Node
	user *models.User
}

func (f *orphanAssignFakeStore) GetServerByUUID(string) (*models.Server, error) {
	return f.server, f.err
}

func (f *orphanAssignFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node != nil {
		return f.node, nil
	}
	return &models.Node{ID: id, Status: "online"}, nil
}

func (f *orphanAssignFakeStore) ListServersByNode(int) ([]models.Server, error) {
	return nil, nil
}

func (f *orphanAssignFakeStore) GetUserByID(string) (*models.User, error) {
	if f.user == nil {
		return nil, sql.ErrNoRows
	}
	return f.user, nil
}

func orphanAssignBody() []byte {
	owner := "b083ff0c-fc16-47c5-9700-7b16486583e7"
	raw, _ := json.Marshal(AssignOrphanRequest{
		NodeID:      2,
		UUID:        "orphan-probe-1",
		Name:        "recovered",
		OwnerUserID: &owner,
		MemoryMB:    1024,
		CPULimit:    1,
	})
	return raw
}

// TestAssignOrphan_DuplicateCheck is the regression guard for a feature that
// could never succeed. "No such server" is the only state a genuine orphan can
// be in, GetServerByUUID reports it as sql.ErrNoRows, and the handler treated
// every error as a database fault - so adopting an orphan answered 500 on the
// one path the endpoint exists for. Live: 409 for a UUID that had a row, 500
// for one that did not.
func TestAssignOrphan_DuplicateCheck(t *testing.T) {
	cases := []struct {
		name     string
		server   *models.Server
		err      error
		wantCode int
	}{
		{
			name:     "no such server is the orphan case and must proceed",
			err:      sql.ErrNoRows,
			wantCode: http.StatusServiceUnavailable, // reaches the gRPC guard below
		},
		{
			name:     "an existing row is a conflict",
			server:   &models.Server{ID: 1},
			wantCode: http.StatusConflict,
		},
		{
			name:     "a real fault stays a fault",
			err:      errors.New("connection refused"),
			wantCode: http.StatusInternalServerError,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// GRPCRegistry stays nil: the orphan case must get PAST the
			// duplicate check and stop at the gRPC guard with 503. Anything
			// else means it was refused earlier.
			h := &NodeHandler{d: testDeps(Deps{Store: &orphanAssignFakeStore{server: c.server, err: c.err}})}

			rw := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/disk/orphans/assign", bytes.NewReader(orphanAssignBody()))
			h.AssignOrphan(rw, req.WithContext(context.WithValue(req.Context(), "isAdmin", true)))

			if rw.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.wantCode, rw.Body.String())
			}
		})
	}
}

// Adopting names the owner, and the owner gets everything on the server, so a
// staff role holding nodes.write could take a deleted customer's world for
// itself. Admin-only, like changing a server's owner.
func TestAssignOrphanIsAdminOnly(t *testing.T) {
	h := &NodeHandler{d: testDeps(Deps{Store: &orphanAssignFakeStore{}})}
	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/disk/orphans/assign", bytes.NewReader(orphanAssignBody()))
	ctx := context.WithValue(req.Context(), "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", "staff-id")
	h.AssignOrphan(rw, req.WithContext(ctx))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rw.Code, rw.Body.String())
	}
}

// Every other path that creates a user runs validate.IsUsername. Adopting an orphan
// did not, and the name it created reaches the node's Redis ACL selector, the beam
// quota key and SFTP, where a space or '+' is not just ugly but a broken grant.
func TestAssignOrphanRejectsInvalidUsername(t *testing.T) {
	for _, name := range []string{"john doe", "x +@all ~*", "a:b"} {
		raw, _ := json.Marshal(map[string]any{
			"node_id":   2,
			"uuid":      "orphan-probe-1",
			"name":      "recovered",
			"new_user":  map[string]string{"username": name, "password": "pw-long-enough"},
			"memory_mb": 1024,
			"cpu_limit": 1,
		})
		h := &NodeHandler{d: testDeps(Deps{Store: &orphanAssignFakeStore{err: sql.ErrNoRows}})}
		rw := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/disk/orphans/assign", bytes.NewReader(raw))
		h.AssignOrphan(rw, req.WithContext(context.WithValue(req.Context(), "isAdmin", true)))
		if rw.Code != http.StatusBadRequest {
			t.Errorf("username %q: status = %d, want 400 (%s)", name, rw.Code, rw.Body.String())
		}
	}
}

// inspectNode answers the adopt's InspectOrphanReq with an empty folder report.
type inspectNode struct {
	grpc.ServerStream
	conn *nodegrpc.NodeConnection
}

func (s *inspectNode) Context() context.Context { return context.Background() }
func (s *inspectNode) Recv() (*pb.NodeMessage, error) {
	select {}
}
func (s *inspectNode) Send(m *pb.NodeMessage) error {
	if m.GetInspectOrphanReq() != nil {
		go s.conn.RouteResponse(&pb.NodeMessage{RequestId: m.RequestId,
			Payload: &pb.NodeMessage_InspectOrphanResp{InspectOrphanResp: &pb.InspectOrphanResp{}}})
	}
	return nil
}

type orphanCreateStore struct {
	orphanAssignFakeStore
	created *models.Server
}

func (f *orphanCreateStore) CreateServer(srv *models.Server) (int64, error) {
	c := *srv
	f.created = &c
	return 9, nil
}
func (f *orphanCreateStore) UpdateServerSetup(int, string, string, string, string, string, string, string) error {
	return nil
}

// An adopted server was stored with an empty region, which CreateServer writes
// as 'default'. servers.region is what the beam relay pick and the rebalancer's
// region fence read, so it must be the node's region like every created server.
func TestAssignOrphanUsesTheNodesRegion(t *testing.T) {
	st := &orphanCreateStore{orphanAssignFakeStore: orphanAssignFakeStore{
		err:  sql.ErrNoRows,
		node: &models.Node{ID: 2, Status: "online", Region: "eu"},
		user: &models.User{ID: "b083ff0c-fc16-47c5-9700-7b16486583e7"},
	}}
	reg := nodegrpc.NewRegistry()
	node := &inspectNode{}
	node.conn = reg.Register(2, "tok", node)
	h := &NodeHandler{d: testDeps(Deps{Store: st, GRPCRegistry: reg})}

	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/disk/orphans/assign", bytes.NewReader(orphanAssignBody()))
	h.AssignOrphan(rw, req.WithContext(context.WithValue(req.Context(), "isAdmin", true)))

	if rw.Code != http.StatusOK || st.created == nil {
		t.Fatalf("status = %d, created = %v (%s)", rw.Code, st.created, rw.Body.String())
	}
	if st.created.Region != "eu" {
		t.Errorf("adopted server region = %q, want the node's region eu", st.created.Region)
	}
}
