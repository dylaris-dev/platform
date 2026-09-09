package services

import (
	"reflect"
	"testing"

	"dylaris-core/models"
)

func srv(id int, uuid string, nodeID int, kind string, proxy *int) models.Server {
	return models.Server{ID: id, UUID: uuid, NodeID: nodeID, ServerType: kind, ProxyID: proxy}
}

func proxyRef(i int) *int { return &i }

// The direction is the whole thing, and it is the easy half to get backwards.
//
// A Bungee/Velocity proxy DIALS its backends; a backend never dials the proxy
// and never dials another backend - everything cross-server rides the proxy's
// existing connection on the bungeecord:main channel. So the BACKEND's ingress
// list names the proxy, and the proxy's list stays empty.
//
// Reversed, the result is not merely useless: it opens the proxy to its
// backends while leaving every backend still refusing the proxy, which takes
// the network down and loosens the thing it was supposed to tighten.
func TestBuildNodePoliciesNamesTheProxyOnTheBackend(t *testing.T) {
	proxyID := 1
	servers := []models.Server{
		srv(1, "uuid-proxy", 10, "proxy", nil),
		srv(2, "uuid-back-a", 10, "game", proxyRef(proxyID)),
		srv(3, "uuid-back-b", 10, "game", proxyRef(proxyID)),
		srv(4, "uuid-lonely", 10, "game", nil),
	}

	got := BuildNodePolicies(servers)
	want := map[string][]string{
		"uuid-proxy":  nil,
		"uuid-back-a": {"uuid-proxy"},
		"uuid-back-b": {"uuid-proxy"},
		"uuid-lonely": nil,
	}
	if !reflect.DeepEqual(got[10], want) {
		t.Errorf("policy for node 10 =\n  %#v\nwant\n  %#v", got[10], want)
	}

	// Said explicitly, because a reversed implementation still passes a test
	// that only checks the backend side.
	if len(got[10]["uuid-proxy"]) != 0 {
		t.Errorf("the proxy was given an allow list (%v); players reach it through the Link, which is unconditional",
			got[10]["uuid-proxy"])
	}
}

// A server on another machine belongs in that machine's policy and nowhere
// else. Publishing one node's servers to another would hand a node addresses
// it has no business allowing, and would leave the real owner's entry missing.
func TestBuildNodePoliciesKeepsNodesApart(t *testing.T) {
	servers := []models.Server{
		srv(1, "uuid-proxy", 10, "proxy", nil),
		srv(2, "uuid-here", 10, "game", proxyRef(1)),
		srv(3, "uuid-there", 20, "game", proxyRef(1)),
	}
	got := BuildNodePolicies(servers)

	if _, ok := got[10]["uuid-there"]; ok {
		t.Error("a server from node 20 appeared in node 10's policy")
	}
	if _, ok := got[20]["uuid-there"]; !ok {
		t.Error("node 20's own server is missing from its policy")
	}
	// Cross-host is the normal case, not an edge case: the proxy is on node 10
	// and its backend on node 20, and one shared overlay is what makes that work.
	if want := []string{"uuid-proxy"}; !reflect.DeepEqual(got[20]["uuid-there"], want) {
		t.Errorf("cross-host backend allow = %v, want %v", got[20]["uuid-there"], want)
	}
}

// A dangling proxy_id must not become a rule. The row can outlive the proxy it
// points at (the link is a plain column), and a "" in the list would travel all
// the way to a node to be discarded there.
func TestBuildNodePoliciesIgnoresADanglingProxy(t *testing.T) {
	servers := []models.Server{srv(2, "uuid-back", 10, "game", proxyRef(999))}
	got := BuildNodePolicies(servers)
	if n := len(got[10]["uuid-back"]); n != 0 {
		t.Errorf("dangling proxy produced %d entries: %v", n, got[10]["uuid-back"])
	}
	if _, ok := got[10]["uuid-back"]; !ok {
		t.Error("the server itself disappeared from the policy")
	}
}

// A server linked to itself is a data anomaly, and the rule it would produce
// is one that allows a container to reach itself - which lo already covers,
// and which would read as a peer allowance nobody made.
func TestBuildNodePoliciesIgnoresASelfLink(t *testing.T) {
	servers := []models.Server{srv(2, "uuid-self", 10, "game", proxyRef(2))}
	got := BuildNodePolicies(servers)
	if n := len(got[10]["uuid-self"]); n != 0 {
		t.Errorf("self-link produced %d entries: %v", n, got[10]["uuid-self"])
	}
}

// Both sides compute this key independently, in two repositories' worth of
// code paths, and a mismatch is silent: the node reads a key nobody writes and
// concludes no policy has been published, which switches enforcement OFF.
func TestNetPolicyKeyIsInsideTheNodeGrant(t *testing.T) {
	token := "597de090-d6b5-4c66-b617-d13b676ecb53"
	got := NetPolicyKey(token)
	if want := "dylaris:node:" + token + ":netpolicy"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}
