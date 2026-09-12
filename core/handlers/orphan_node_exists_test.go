package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// orphanNodeFakeStore embeds store.Store (nil) so it satisfies the interface at
// compile time. A missing node must be refused before anything else runs; a
// known one then meets the orphan check, which asks whether the uuid has a
// server row.
type orphanNodeFakeStore struct {
	store.Store
	node *models.Node
	err  error
}

func (f *orphanNodeFakeStore) GetNodeByID(int) (*models.Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.node, nil
}

// No row: "some-orphan" really is one, so the known-node case below measures
// the node check and not the orphan check.
func (f *orphanNodeFakeStore) GetServerByUUID(string) (*models.Server, error) {
	return nil, sql.ErrNoRows
}

func orphanReq(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	return mux.SetURLVars(req, map[string]string{"nodeId": "999999", "uuid": "some-orphan"})
}

// TestOrphanEndpoints_UnknownNodeIs404 pins all four orphan endpoints. They
// took the nodeId straight from the path and went on to the gRPC call, so an id
// no node has came back 502 "node 999999 not connected" - observed live in the
// route sweep. That message points an operator at a connectivity fault on a
// node that does not exist.
//
// GRPCRegistry is deliberately left nil: if the node check is skipped the
// handler reaches the registry guard and answers 503, so a wrong status here
// still fails the test rather than accidentally passing.
func TestOrphanEndpoints_UnknownNodeIs404(t *testing.T) {
	h := &NodeHandler{state: &AppState{Store: &orphanNodeFakeStore{err: errors.New("sql: no rows in result set")}}}

	cases := []struct {
		name   string
		call   func(http.ResponseWriter, *http.Request)
		method string
		target string
	}{
		{"ListOrphanFiles", h.ListOrphanFiles, http.MethodGet, "/api/disk/orphans/999999/some-orphan/files"},
		{"GetOrphanFileContent", h.GetOrphanFileContent, http.MethodGet, "/api/disk/orphans/999999/some-orphan/content?path=x"},
		{"InspectOrphan", h.InspectOrphan, http.MethodGet, "/api/disk/orphans/999999/some-orphan/inspect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rw := httptest.NewRecorder()
			c.call(rw, orphanReq(c.method, c.target))
			if rw.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
			}
		})
	}
}

// A node that DOES exist must get past the check, so the guard cannot be a
// blanket refusal. With the registry nil the next stop is the 503, which is
// exactly the proof that the node check let it through.
func TestOrphanEndpoints_KnownNodePassesTheCheck(t *testing.T) {
	h := &NodeHandler{state: &AppState{Store: &orphanNodeFakeStore{node: &models.Node{ID: 2}}}}

	rw := httptest.NewRecorder()
	h.InspectOrphan(rw, orphanReq(http.MethodGet, "/api/disk/orphans/999999/some-orphan/inspect"))
	if rw.Code == http.StatusNotFound {
		t.Fatalf("an existing node was refused with 404 (%s)", rw.Body.String())
	}
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 from the gRPC-registry guard (%s)", rw.Code, rw.Body.String())
	}
}
