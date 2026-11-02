package access

import (
	"errors"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
)

// nodeReadCounter is ForeignStore counting node reads.
type nodeReadCounter struct {
	*handlertest.ForeignStore
	nodeReads int
}

func (c *nodeReadCounter) GetNodeByID(id int) (*models.Node, error) {
	c.nodeReads++
	return c.ForeignStore.GetNodeByID(id)
}

// A row that carries its node's owner answers without reading the node, and
// gives the same answer the node read would; a row that does not falls back,
// and an unreadable node still counts as foreign.
func TestServerNodeOwnedByOther(t *testing.T) {
	owner := "customer"
	for _, tc := range []struct {
		name      string
		srv       *models.Server
		nodeErr   error
		user      string
		want      bool
		wantReads int
	}{
		{"joined, customer's node, admin asks", &models.Server{NodeID: 7, NodeOwnerID: &owner, NodeOwnerLoaded: true}, nil, "admin", true, 0},
		{"joined, customer's node, customer asks", &models.Server{NodeID: 7, NodeOwnerID: &owner, NodeOwnerLoaded: true}, nil, owner, false, 0},
		{"joined, platform node", &models.Server{NodeID: 7, NodeOwnerLoaded: true}, nil, "admin", false, 0},
		{"not joined, falls back to the node", &models.Server{NodeID: 7}, nil, "admin", true, 1},
		{"not joined, node unreadable", &models.Server{NodeID: 7}, errors.New("db down"), "admin", true, 1},
		{"no row", nil, nil, "admin", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &nodeReadCounter{ForeignStore: &handlertest.ForeignStore{Node: &models.Node{ID: 7, OwnerID: &owner}, NodeErr: tc.nodeErr, Byon: "true"}}
			got := ServerNodeOwnedByOther(st, services.NewFeatureFlags(st), tc.srv, tc.user)
			if got != tc.want || st.nodeReads != tc.wantReads {
				t.Fatalf("got %v with %d node reads, want %v with %d", got, st.nodeReads, tc.want, tc.wantReads)
			}
		})
	}
}
