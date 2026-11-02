package health

import (
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func ptr(s string) *string { return &s }

// customerHealthStore serves nodes and the route-only link rows.
type customerHealthStore struct {
	store.Store
	nodes  []models.Node
	routes []store.CoreLinkRoute
}

func (f *customerHealthStore) ListNodes() ([]models.Node, error) { return f.nodes, nil }
func (f *customerHealthStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return f.routes, nil
}

// The status page answers "is MY platform healthy". A customer's BYON machine
// being off is not part of that answer, and counting it turned the page amber
// for hardware nobody here can reach - which is how an operator learns to stop
// reading the page.
func TestTheStatusPageIgnoresCustomerNodes(t *testing.T) {
	h := NewHealthHandler(Deps{Store: &customerHealthStore{nodes: []models.Node{
		{ID: 1, Name: "cluster-a", Token: "t1", Status: "online"},
		{ID: 2, Name: "external-a", Token: "t2", Status: "online", Tags: "eu,external"},
		// Two customer machines, one of them down. Neither may move the needle.
		{ID: 3, Name: "byon-a", Token: "t3", Status: "online", OwnerID: ptr("u-1")},
		{ID: 4, Name: "byon-b", Token: "t4", Status: "offline", OwnerID: ptr("u-2")},
	}}})

	comp := h.nodesComponent()
	if comp.Status != "up" {
		t.Fatalf("status = %q with only a CUSTOMER node offline; want up", comp.Status)
	}
	if comp.Detail != "2/2 online" {
		t.Fatalf("detail = %q; want 2/2 online (cluster + external, no BYON)", comp.Detail)
	}
	for _, item := range comp.Items {
		if strings.HasPrefix(item.Name, "byon") {
			t.Errorf("customer node %q was listed on the status page", item.Name)
		}
	}
}

// An external node IS the operator's - they registered it and they are
// responsible for it. Only ownership makes a machine somebody else's.
func TestTheStatusPageStillCountsExternalNodes(t *testing.T) {
	h := NewHealthHandler(Deps{Store: &customerHealthStore{nodes: []models.Node{
		{ID: 1, Name: "cluster-a", Token: "t1", Status: "online"},
		{ID: 2, Name: "external-a", Token: "t2", Status: "offline", Tags: "external"},
	}}})

	comp := h.nodesComponent()
	if comp.Status != "degraded" {
		t.Fatalf("status = %q with an EXTERNAL node offline; want degraded", comp.Status)
	}
	if comp.Detail != "1/2 online" {
		t.Fatalf("detail = %q; want 1/2 online", comp.Detail)
	}
}

// The case that would otherwise read as a broken platform: every machine here
// belongs to a customer, so there is genuinely nothing of ours registered - and
// the reason has to say so, because the operator can SEE nodes in the panel.
func TestAFleetOfOnlyCustomerNodesSaysWhyItLooksEmpty(t *testing.T) {
	h := NewHealthHandler(Deps{Store: &customerHealthStore{nodes: []models.Node{
		{ID: 1, Name: "byon-a", Token: "t1", Status: "online", OwnerID: ptr("u-1")},
	}}})

	comp := h.nodesComponent()
	if comp.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", comp.Status)
	}
	if !strings.Contains(comp.Reason, "customer node") {
		t.Fatalf("reason %q does not explain that customer nodes are excluded", comp.Reason)
	}
}
