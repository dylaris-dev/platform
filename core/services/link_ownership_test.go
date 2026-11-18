package services

import (
	"dylaris-core/models"
	"dylaris-core/store"
	"errors"
	"testing"
)

type ownershipStore struct {
	store.Store
	nodes     []models.Node
	routes    []store.CoreLinkRoute
	nodeErr   error
	routesErr error
}

func (s *ownershipStore) ListNodes() ([]models.Node, error) { return s.nodes, s.nodeErr }
func (s *ownershipStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return s.routes, s.routesErr
}

func own(id string) *string { return &id }

func TestALinkIsCustomerOnlyWhenTheDatabaseSaysSo(t *testing.T) {
	// Redis knows nothing about ownership: a link registers under its own token
	// and carries no owner, so the operator's DC link and a customer's kit look
	// identical in the live list. The database is the only place that can tell
	// them apart, in exactly two spots.
	st := &ownershipStore{
		nodes: []models.Node{
			{Token: "p1", LinkSecret: "link-platform"},
			{Token: "e1", Tags: "eu,external", LinkSecret: "link-external"},
			{Token: "b1", OwnerID: own("u-1"), LinkSecret: "link-byon"},
			// A BYON node with no link sidecar contributes no token.
			{Token: "b2", OwnerID: own("u-2")},
		},
		routes: []store.CoreLinkRoute{{Domain: "a.example", OwnerID: "u-3", LinkToken: "link-routeonly"}},
	}
	o := LoadLinkOwnership(st, "cs")

	for _, tok := range []string{"link-byon", "link-routeonly"} {
		if !o.IsCustomer(tok) {
			t.Errorf("%s is a customer's and was counted as ours", tok)
		}
	}
	// An external node is the OPERATOR's hardware - registered by them, their
	// responsibility. Only ownership makes a node a customer's.
	for _, tok := range []string{"link-platform", "link-external", "", "link-unknown"} {
		if o.IsCustomer(tok) {
			t.Errorf("%q was counted as a customer's", tok)
		}
	}
}

func TestAFailedLookupCountsEverythingAsOurs(t *testing.T) {
	// The conservative direction. Classifying one of ours as a customer's would
	// drop a real outage of ours out of the status page silently; the other way
	// round can only raise an alarm somebody then dismisses.
	o := LoadLinkOwnership(&ownershipStore{nodeErr: errors.New("db down")}, "cs")
	if o.IsCustomer("anything") {
		t.Fatal("a failed lookup classified a link as a customer's")
	}
}

func TestRoutesFailingKeepsWhatTheNodesAlreadySaid(t *testing.T) {
	// Two reads, and losing the second must not discard the first.
	st := &ownershipStore{
		nodes:     []models.Node{{Token: "b1", OwnerID: own("u-1"), LinkSecret: "link-byon"}},
		routesErr: errors.New("db down"),
	}
	o := LoadLinkOwnership(st, "cs")
	if !o.IsCustomer("link-byon") {
		t.Fatal("a route lookup failure discarded the node classification")
	}
}

func TestSplitLinksCountsEachSideSeparately(t *testing.T) {
	st := &ownershipStore{
		nodes:  []models.Node{{Token: "b1", OwnerID: own("u-1"), LinkSecret: "cust-a"}},
		routes: []store.CoreLinkRoute{{LinkToken: "cust-b"}},
	}
	s := LoadLinkOwnership(st, "cs").SplitLinks([]GatewayLinkStatus{
		{Token: "ours-1", Online: true},
		{Token: "ours-2", Online: false},
		{Token: "cust-a", Online: true},
		{Token: "cust-b", Online: false},
	})

	if len(s.Ours) != 2 || s.OursOnline != 1 {
		t.Errorf("ours = %d links, %d online; want 2 and 1", len(s.Ours), s.OursOnline)
	}
	if len(s.Customer) != 2 || s.CustomerOnline != 1 {
		t.Errorf("customer = %d links, %d online; want 2 and 1", len(s.Customer), s.CustomerOnline)
	}
	// The number that matters: a customer's link being down must not be able to
	// make the operator's own count look incomplete.
	if s.OursOnline != len(s.Ours)-1 {
		t.Errorf("a customer link leaked into the operator's own count")
	}
}

func TestSplitNodesKeepsExternalOnOurSide(t *testing.T) {
	// External is the operator's own hardware outside the cluster. Only an
	// owner makes a node somebody else's - and an owned node that is ALSO
	// tagged external is still theirs, which is the case that decides it.
	ours, customer := SplitNodes([]models.Node{
		{Token: "p1"},
		{Token: "e1", Tags: "external"},
		{Token: "b1", OwnerID: own("u-1")},
		{Token: "b2", OwnerID: own("u-2"), Tags: "external"},
	})
	if len(ours) != 2 {
		t.Errorf("ours = %d, want 2 (platform + external)", len(ours))
	}
	if len(customer) != 2 {
		t.Errorf("customer = %d, want 2 (both owned)", len(customer))
	}
	for _, n := range customer {
		if n.OwnerID == nil {
			t.Error("an unowned node was put on the customer side")
		}
	}
}

func TestABYONLinkIsCustomerByTheTokenItActuallyRunsUnder(t *testing.T) {
	// CreateBYONNode writes token + owner and no link_secret, so the link of a
	// normally enrolled BYON machine runs under the derived token, or under the
	// one the Hub named for it once it has answered.
	const cs = "cluster-secret"
	st := &ownershipStore{
		nodes: []models.Node{
			{Token: "b1", OwnerID: own("u-1")},
			{Token: "b2", OwnerID: own("u-2"), LinkToken: "hub-named"},
			{Token: "p1"},
		},
	}
	o := LoadLinkOwnership(st, cs)
	for _, tok := range []string{DeriveLinkToken("b1", cs), "hub-named"} {
		if !o.IsCustomer(tok) {
			t.Errorf("BYON link %q was counted as ours", tok)
		}
	}
	if o.IsCustomer(DeriveLinkToken("p1", cs)) {
		t.Error("a platform node's link was counted as a customer's")
	}
}
