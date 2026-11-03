package health

import (
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func ptr(s string) *string { return &s }

// fleet returns the cluster and the external component, in that order.
func fleet(t *testing.T, h *HealthHandler) (cluster, external healthComponent) {
	t.Helper()
	comps := h.nodeComponents()
	if len(comps) != 2 || comps[0].Key != "nodes" || comps[1].Key != "external_nodes" {
		t.Fatalf("components = %+v, want nodes then external_nodes", comps)
	}
	return comps[0], comps[1]
}

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

	cluster, external := fleet(t, h)
	for _, comp := range []healthComponent{cluster, external} {
		if comp.Status != "up" || comp.Detail != "1/1 online" {
			t.Errorf("%s = %q %q with only a CUSTOMER node offline; want up 1/1 online", comp.Key, comp.Status, comp.Detail)
		}
		for _, item := range comp.Items {
			if strings.HasPrefix(item.Name, "byon") {
				t.Errorf("customer node %q was listed on the status page", item.Name)
			}
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

	cluster, external := fleet(t, h)
	if cluster.Status != "up" || cluster.Detail != "1/1 online" {
		t.Fatalf("cluster = %q %q; the external node's outage leaked into it", cluster.Status, cluster.Detail)
	}
	if external.Status != "down" || external.Detail != "0/1 online" {
		t.Fatalf("external = %q %q with its only node offline; want down 0/1 online", external.Status, external.Detail)
	}
}

// The case that would otherwise read as a broken platform: every machine here
// belongs to a customer, so there is genuinely nothing of ours registered - and
// the reason has to say so, because the operator can SEE nodes in the panel.
func TestAFleetOfOnlyCustomerNodesSaysWhyItLooksEmpty(t *testing.T) {
	h := NewHealthHandler(Deps{Store: &customerHealthStore{nodes: []models.Node{
		{ID: 1, Name: "byon-a", Token: "t1", Status: "online", OwnerID: ptr("u-1")},
	}}})

	comp, external := fleet(t, h)
	if external.Status != "disabled" {
		t.Fatalf("external = %q with none registered; want disabled", external.Status)
	}
	if comp.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", comp.Status)
	}
	if !strings.Contains(comp.Reason, "customer node") {
		t.Fatalf("reason %q does not explain that customer nodes are excluded", comp.Reason)
	}
}

// A platform that runs only on external machines is not missing anything: an
// empty cluster is not a fault while external nodes carry the servers.
func TestAnEmptyClusterIsFineWhenExternalNodesExist(t *testing.T) {
	h := NewHealthHandler(Deps{Store: &customerHealthStore{nodes: []models.Node{
		{ID: 1, Name: "external-a", Token: "t1", Status: "online", Tags: "external"},
	}}})
	cluster, external := fleet(t, h)
	if cluster.Status != "disabled" {
		t.Fatalf("cluster = %q with external nodes carrying the load; want disabled", cluster.Status)
	}
	if external.Status != "up" {
		t.Fatalf("external = %q; want up", external.Status)
	}
}
