package redisacl

import (
	"sort"
	"strings"
	"testing"
)

func TestExpectedNodeACLUsersCoversAllThree(t *testing.T) {
	// One shipper user PER SERVER, so a node with two servers has four users.
	got := ExpectedNodeACLUsers(map[string][]string{"tok-a": {"srv-1", "srv-2"}})
	for _, want := range []string{
		"node-tok-a", "node-tok-a-link",
		"node-tok-a-shipper-srv-1", "node-tok-a-shipper-srv-2",
	} {
		if !got[want] {
			t.Errorf("expected set is missing %q", want)
		}
	}
	if len(got) != 4 {
		t.Errorf("expected set has %d entries, want 4: %v", len(got), got)
	}
	// The old fleet-wide name must NOT be expected any more, or the prune would
	// keep resurrecting a credential that reaches every server on the node.
	if got["node-tok-a-shipper"] {
		t.Error("the per-node shipper user is still in the expected set")
	}
}

// A server that MOVED to another node must lose its shipper credential on the
// old one. Nothing tracks that explicitly - it falls out of the expected set
// being rebuilt from the authoritative server list, and the prune doing the rest.
func TestExpectedNodeACLUsersDropsMovedServer(t *testing.T) {
	before := ExpectedNodeACLUsers(map[string][]string{"tok-a": {"srv-1"}})
	after := ExpectedNodeACLUsers(map[string][]string{"tok-a": {}})
	moved := ShipperUsername("tok-a", "srv-1")
	if !before[moved] {
		t.Fatalf("%q should have been expected while the server was placed there", moved)
	}
	if after[moved] {
		t.Errorf("%q is still expected after the server left the node", moved)
	}
}

// TestUnknownNodeACLUsers is the safety surface of the prune: everything it
// returns gets DELUSER'd, so a false positive deletes a live node's credential.
func TestUnknownNodeACLUsers(t *testing.T) {
	live := "aaaaaaaa-1111-2222-3333-444444444444"
	dead := "bbbbbbbb-5555-6666-7777-888888888888"
	expected := ExpectedNodeACLUsers(map[string][]string{live: {"srv-1"}})

	users := []string{
		"default",
		// A link- user from before route-only links lost their Redis login
		// (generateLinkIdentity returns "link-"+hex). It is outside the node-
		// namespace, so the prune must leave it alone like any other stranger.
		"link-0123456789abcdef0123456789abcdef",
		// Other subsystems on the same Redis.
		"gw-warp",
		"hub-admin",
		NodeUsername(live), ShipperUsername(live, "srv-1"), LinkUsername(live),
		NodeUsername(dead), ShipperUsername(dead, "srv-9"), LinkUsername(dead),
	}

	got := UnknownNodeACLUsers(users, expected)
	sort.Strings(got)
	want := []string{NodeUsername(dead), LinkUsername(dead), ShipperUsername(dead, "srv-9")}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("got %d orphans %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("orphan[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Stated separately from the count so a regression says WHICH invariant broke.
	for _, u := range got {
		if strings.Contains(u, live) {
			t.Errorf("prune would delete %q, which belongs to a live node", u)
		}
		if !strings.HasPrefix(u, nodeACLPrefix) {
			t.Errorf("prune would delete %q, outside Core's node- namespace", u)
		}
	}
}

// TestUnknownNodeACLUsersEmptyExpected pins the shape the reconciler's own guard
// exists for: with nothing expected, EVERY node- user is an orphan. That is
// correct here and is exactly why the caller refuses to run this on an empty
// node list.
func TestUnknownNodeACLUsersEmptyExpected(t *testing.T) {
	users := []string{"default", "link-abc", "node-x", "node-x-link"}
	got := UnknownNodeACLUsers(users, map[string]bool{})
	if len(got) != 2 {
		t.Fatalf("got %v, want both node- users", got)
	}
}
