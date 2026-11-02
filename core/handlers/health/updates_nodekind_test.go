package health

import (
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type updNodeStore struct {
	store.Store
	nodes []models.Node
}

func (s *updNodeStore) ListNodes() ([]models.Node, error) { return s.nodes, nil }

func updHandler(nodes []models.Node) *UpdatesHandler {
	return &UpdatesHandler{d: Deps{Store: &updNodeStore{nodes: nodes}}}
}

func blockByKey(blocks []componentBlock, key string) (componentBlock, bool) {
	for _, b := range blocks {
		if b.Key == key {
			return b, true
		}
	}
	return componentBlock{}, false
}

var mixedFleet = []models.Node{
	{ID: 1, Name: "cluster-a", Token: "t1"},
	{ID: 2, Name: "cluster-b", Token: "t2"},
	{ID: 3, Name: "external-a", Token: "t3", Tags: "eu,external"},
	{ID: 4, Name: "byon-a", Token: "t4", OwnerID: ptr("u-1")},
	{ID: 5, Name: "byon-b", Token: "t5", OwnerID: ptr("u-2"), Tags: "external"},
}

// What's New answers "is anything of MINE behind". A customer's BYON node is
// their update to install, announced to them through their own feed - putting it
// here made the operator's answer depend on hardware they cannot touch.
func TestAnAdminsUpdateViewLeavesOutCustomerNodes(t *testing.T) {
	h := updHandler(mixedFleet)
	r := httptest.NewRequest("GET", "/api/updates", nil).WithContext(adminCtx("admin-1", true))

	blocks := h.components(r, mustParse(t, notesForTest), "admin-1", true)

	cluster, ok := blockByKey(blocks, "node")
	if !ok {
		t.Fatal("no cluster node row")
	}
	if len(cluster.Instances) != 2 {
		var names []string
		for _, i := range cluster.Instances {
			names = append(names, i.Label)
		}
		t.Fatalf("cluster row holds %v; want only the two cluster nodes", names)
	}
	for _, b := range blocks {
		for _, i := range b.Instances {
			if i.Label == "byon-a" || i.Label == "byon-b" {
				t.Errorf("customer node %q appears in row %q", i.Label, b.Key)
			}
		}
	}
}

// The operator's own machines outside the swarm are theirs to update, so they
// belong here - but counted apart, because "2/2 cluster nodes current" and
// "1 external node behind" are two different pieces of work.
func TestExternalNodesAreTheirOwnRow(t *testing.T) {
	h := updHandler(mixedFleet)
	r := httptest.NewRequest("GET", "/api/updates", nil).WithContext(adminCtx("admin-1", true))

	blocks := h.components(r, mustParse(t, notesForTest), "admin-1", true)

	ext, ok := blockByKey(blocks, "node-external")
	if !ok {
		t.Fatal("no external node row")
	}
	if len(ext.Instances) != 1 || ext.Instances[0].Label != "external-a" {
		t.Fatalf("external row = %+v; want just external-a", ext.Instances)
	}
	if ext.Label == "" {
		t.Error("the external row has no label, so the panel would call it by the service name")
	}
	// Both rows report against the SAME service: a release names `node` and
	// both kinds install it, so "what should it be on" must be one answer.
	cluster, _ := blockByKey(blocks, "node")
	if ext.Service != "node" || cluster.Service != "node" {
		t.Errorf("services = %q and %q; both node rows must report against `node`", cluster.Service, ext.Service)
	}
	if ext.Latest != cluster.Latest {
		t.Errorf("latest differs between the node rows: %q vs %q", cluster.Latest, ext.Latest)
	}
}

// An operator with no external machines gets no external row. An always-present
// empty one reads as a component that is missing rather than one that does not
// apply here.
func TestNoExternalRowWithoutExternalNodes(t *testing.T) {
	h := updHandler([]models.Node{{ID: 1, Name: "cluster-a", Token: "t1"}})
	r := httptest.NewRequest("GET", "/api/updates", nil).WithContext(adminCtx("admin-1", true))

	blocks := h.components(r, mustParse(t, notesForTest), "admin-1", true)
	if _, ok := blockByKey(blocks, "node-external"); ok {
		t.Fatal("an external row was drawn for an operator with no external nodes")
	}
	if _, ok := blockByKey(blocks, "node"); !ok {
		t.Fatal("the cluster row disappeared")
	}
}

// The other audience, unchanged: a BYON customer's whole update view IS their
// own hardware. Excluding BYON is about the ADMIN's view, and reusing the same
// filter for both would leave the customer with nothing to look at - which is
// the hole the hosted feed was built to close.
func TestACustomerStillSeesTheirOwnNodes(t *testing.T) {
	h := updHandler(mixedFleet)
	r := httptest.NewRequest("GET", "/api/updates", nil).WithContext(adminCtx("u-1", false))

	blocks := h.components(r, mustParse(t, notesForTest), "u-1", false)

	row, ok := blockByKey(blocks, "node")
	if !ok {
		t.Fatal("no node row for the customer")
	}
	if len(row.Instances) != 1 || row.Instances[0].Label != "byon-a" {
		t.Fatalf("customer sees %+v; want only their own byon-a", row.Instances)
	}
	// And nothing of anybody else's, including the operator's cluster.
	for _, b := range blocks {
		for _, i := range b.Instances {
			if i.Label != "byon-a" {
				t.Errorf("customer sees %q in row %q", i.Label, b.Key)
			}
		}
	}
}

// Every row needs a key, and no two may share one: the panel uses it as a React
// key, and the requirement scan uses it to avoid announcing one mandatory node
// update twice.
func TestEveryRowHasAUniqueKey(t *testing.T) {
	h := updHandler(mixedFleet)
	r := httptest.NewRequest("GET", "/api/updates", nil).WithContext(adminCtx("admin-1", true))

	seen := map[string]bool{}
	for _, b := range h.components(r, mustParse(t, notesForTest), "admin-1", true) {
		if b.Key == "" {
			t.Errorf("row for service %q has no key", b.Service)
		}
		if seen[b.Key] {
			t.Errorf("duplicate row key %q", b.Key)
		}
		seen[b.Key] = true
	}
}
