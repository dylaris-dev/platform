package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
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

func (f *orphanAssignFakeStore) GetUserRegionIDs(string) ([]string, error) {
	return nil, nil
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
			h := &NodeHandler{state: &AppState{Store: &orphanAssignFakeStore{server: c.server, err: c.err}}}

			rw := httptest.NewRecorder()
			h.AssignOrphan(rw, httptest.NewRequest(http.MethodPost, "/api/disk/orphans/assign", bytes.NewReader(orphanAssignBody())))

			if rw.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.wantCode, rw.Body.String())
			}
		})
	}
}
