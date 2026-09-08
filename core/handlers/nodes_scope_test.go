package handlers

import (
	"testing"

	"dylaris-core/models"
)

// The bug these exist to fix: GetNodes returned the whole fleet to an admin, so
// swarm hosts showed up under "my machines" next to the customer's own hardware.
// A swarm host must land in NEITHER of the two kinds the page can name.
func TestNodeScopePredicates(t *testing.T) {
	owner := "11111111-1111-1111-1111-111111111111"

	tests := []struct {
		name     string
		node     models.Node
		external bool
		byon     bool
	}{
		{
			name:     "a swarm host is neither",
			node:     models.Node{Name: "swarm-1"},
			external: false,
			byon:     false,
		},
		{
			name:     "a tagged swarm host is still not external if it has no tag",
			node:     models.Node{Name: "swarm-2", Tags: "eu,ssd"},
			external: false,
			byon:     false,
		},
		{
			name:     "the operator's own external machine",
			node:     models.Node{Name: "office-box", Tags: "external"},
			external: true,
			byon:     false,
		},
		{
			name:     "external among other tags",
			node:     models.Node{Name: "office-box", Tags: "eu,external,ssd"},
			external: true,
			byon:     false,
		},
		{
			// The one that must not cross over: a customer machine is external
			// too, but it belongs to the customer's tab, not the operator's.
			name:     "a customer machine is BYON, never external",
			node:     models.Node{Name: "home-desktop", Tags: "external", OwnerID: &owner},
			external: false,
			byon:     true,
		},
		{
			// A BYON node that has not reconnected since nodes started reporting
			// the tag. Still the customer's, still on the BYON tab.
			name:     "an untagged owned node is still BYON",
			node:     models.Node{Name: "home-desktop", OwnerID: &owner},
			external: false,
			byon:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExternalPlatformNode(tt.node); got != tt.external {
				t.Errorf("isExternalPlatformNode = %v, want %v", got, tt.external)
			}
			if got := isBYONNode(tt.node); got != tt.byon {
				t.Errorf("isBYONNode = %v, want %v", got, tt.byon)
			}
			if isExternalPlatformNode(tt.node) && isBYONNode(tt.node) {
				t.Error("a node landed in both tabs; the two kinds must be disjoint")
			}
		})
	}
}

func TestFilterNodesKeepsOrderAndNeverReturnsNil(t *testing.T) {
	owner := "22222222-2222-2222-2222-222222222222"
	in := []models.Node{
		{Name: "a", OwnerID: &owner},
		{Name: "b"},
		{Name: "c", OwnerID: &owner},
	}
	got := filterNodes(in, isBYONNode)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("filterNodes = %+v, want a then c", got)
	}
	// The panel reads res.nodes as an array; a null would make it fall over.
	if empty := filterNodes(in, isExternalPlatformNode); empty == nil {
		t.Error("filterNodes returned nil, want an empty slice so the JSON stays []")
	}
}

// "Your machines" means MINE, and that has to hold for an admin too.
//
// Measured on a live install: the same BYON machine appeared in the customer's
// account and again in the admin's, both under the heading "Your machines". The
// non-admin branch of GetNodes already narrowed by owner, so scope=byon only
// filtered by KIND - it looked like it finished the job while enforcing the
// unprivileged half of it. An admin who wants the fleet asks for the unscoped
// list, which is still theirs.
func TestOwnedBYONNodeIsScopedToTheCaller(t *testing.T) {
	me := "11111111-1111-1111-1111-111111111111"
	someoneElse := "22222222-2222-2222-2222-222222222222"

	nodes := []models.Node{
		{Name: "my-desktop", OwnerID: &me},
		{Name: "a-customers-box", OwnerID: &someoneElse},
		{Name: "swarm-1"},
		{Name: "office-box", Tags: "external"},
	}

	got := filterNodes(nodes, ownedBYONNode(me))
	if len(got) != 1 || got[0].Name != "my-desktop" {
		t.Fatalf("scope=byon returned %+v, want only my-desktop - another tenant's machine is listed as mine", got)
	}

	// The other direction, which is the one that was actually broken: asking as
	// somebody who owns nothing returns nothing, not everything.
	if got := filterNodes(nodes, ownedBYONNode("33333333-3333-3333-3333-333333333333")); len(got) != 0 {
		t.Errorf("a caller who owns no machine got %+v", got)
	}
}

// No identity on the request is not a wildcard. Reading the caller out of the
// context can yield "", and a predicate that treated that as "match anything"
// would hand the whole fleet to an unauthenticated path.
func TestOwnedBYONNodeMatchesNothingWithoutACaller(t *testing.T) {
	owner := "44444444-4444-4444-4444-444444444444"
	nodes := []models.Node{{Name: "someones-box", OwnerID: &owner}}
	if got := filterNodes(nodes, ownedBYONNode("")); len(got) != 0 {
		t.Errorf("an empty caller matched %+v", got)
	}
}

// The placement picker must offer exactly what canPlaceOnNode will accept.
//
// It did not: the wizard asked for the unscoped list, so an operator was shown
// every tenant's machine as a target for a new server - hardware that customer
// has root on. Auto-placement never did this (applyPlacementScope sets
// PlatformOnly for an admin), which is what made the gap easy to miss: the same
// question had two answers depending on whether you picked the node yourself.
func TestPlaceableNodeOffersOnlyWhatPlacementAccepts(t *testing.T) {
	me := "11111111-1111-1111-1111-111111111111"
	someoneElse := "22222222-2222-2222-2222-222222222222"

	platform := models.Node{Name: "swarm-1"}
	external := models.Node{Name: "office-box", Tags: "external"}
	mine := models.Node{Name: "my-desktop", Tags: "external", OwnerID: &me}
	theirs := models.Node{Name: "their-desktop", Tags: "external", OwnerID: &someoneElse}

	tests := []struct {
		name  string
		uid   string
		admin bool
		byon  bool
		node  models.Node
		want  bool
	}{
		{"an admin gets platform hardware", me, true, true, platform, true},
		{"an admin gets the operator's own external box", me, true, true, external, true},
		{"an admin does NOT get another tenant's machine", me, true, true, theirs, false},
		{"an admin DOES get a machine they own themselves", me, true, true, mine, true},
		{"a tenant gets their own machine", me, false, true, mine, true},
		{"a tenant does not get someone else's", me, false, true, theirs, false},
		{"a tenant does not get platform hardware", me, false, true, platform, false},
		{"a caller with no identity gets nothing owned", "", false, true, mine, false},
		// With BYON off an owner_id is a leftover, not a tenancy. Something must
		// still be able to place there or the node is stranded.
		{"BYON off: an admin gets an owned node back", me, true, false, theirs, true},
		{"BYON off: a non-admin still gets nothing", me, false, false, mine, false},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			if got := placeableNode(c.uid, c.admin, c.byon)(c.node); got != c.want {
				t.Errorf("placeableNode(%q, admin=%v, byon=%v)(%s) = %v, want %v",
					c.uid, c.admin, c.byon, c.node.Name, got, c.want)
			}
		})
	}
}

// scope=fleet is what Settings -> Nodes asks for: the machines the OPERATOR
// runs. That screen offers Configure, Reset pairing and the deploy bundle, and
// it used to ask for the unscoped list - so every tenant's machine appeared
// there with those buttons live beside it.
//
// Both halves matter. Platform AND external must survive, because external is
// the operator's own hardware outside the swarm and dropping it would hide real
// capacity; every BYON node must go, whoever owns it, including the admin's own
// - Settings is where the platform is administered, and a machine an admin
// brought as a customer is managed from the customer surface like anyone's.
func TestFleetScopeKeepsTheOperatorsMachinesAndNoCustomers(t *testing.T) {
	me := "11111111-1111-1111-1111-111111111111"
	someoneElse := "22222222-2222-2222-2222-222222222222"

	nodes := []models.Node{
		{Name: "swarm-1"},
		{Name: "office-box", Tags: "external"},
		{Name: "my-own-byon", OwnerID: &me},
		{Name: "a-customers-box", OwnerID: &someoneElse},
		// Ownership is asked before the tag, so this is a customer's machine
		// that happens to be tagged - not an external one.
		{Name: "customer-box-tagged", OwnerID: &someoneElse, Tags: "external"},
	}

	got := filterNodes(nodes, func(n models.Node) bool { return !isBYONNode(n) })

	var names []string
	for _, n := range got {
		names = append(names, n.Name)
	}
	if len(names) != 2 || names[0] != "swarm-1" || names[1] != "office-box" {
		t.Fatalf("scope=fleet returned %v, want [swarm-1 office-box]", names)
	}
}
