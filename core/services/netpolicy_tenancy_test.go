package services

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func ownedSrv(id int, uuid string, nodeID int, owner string, proxy *int) models.Server {
	s := srv(id, uuid, nodeID, "game", proxy)
	s.OwnerID = owner
	return s
}

// On a machine shared by several owners every server accepts the other
// servers of its own owner on that machine, and nobody else's.
func TestSameOwnerPoliciesFollowOwnerLines(t *testing.T) {
	servers := []models.Server{
		ownedSrv(1, "a1", 7, "alice", nil),
		ownedSrv(2, "a2", 7, "alice", nil),
		ownedSrv(3, "b1", 7, "bob", nil),
		ownedSrv(4, "a3-elsewhere", 8, "alice", nil),
	}
	got := SameOwnerPolicies(servers, 7)
	for k := range got {
		sort.Strings(got[k])
	}
	want := map[string][]string{
		"a1": {"a2"},
		"a2": {"a1"},
		"b1": {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SameOwnerPolicies = %v, want %v", got, want)
	}
}

type fakeNetPolicyStore struct {
	nodes   []models.Node
	servers []models.Server
}

func (f *fakeNetPolicyStore) ListNodes() ([]models.Node, error)           { return f.nodes, nil }
func (f *fakeNetPolicyStore) ListServers(string) ([]models.Server, error) { return f.servers, nil }

// The flag follows tenancy: set for an external or BYON machine with two
// owners, cleared once it has one, never set on a platform node. A
// single-owner BYON machine still gets its owner's policy (H48): a node with
// NETPOLICY=on separates without the flag, and waited forever for one, or
// kept enforcing the list left from a shared period.
func TestThePublisherFlagsSharedMachinesOnly(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	owner := "carol"
	store := &fakeNetPolicyStore{
		nodes: []models.Node{
			{ID: 1, Token: "plat"},
			{ID: 2, Token: "ext", Tags: "external"},
			{ID: 3, Token: "byon", OwnerID: &owner},
		},
		servers: []models.Server{
			ownedSrv(1, "p1", 1, "alice", nil), ownedSrv(2, "p2", 1, "bob", nil),
			ownedSrv(3, "e1", 2, "alice", nil), ownedSrv(4, "e2", 2, "bob", nil), ownedSrv(5, "e3", 2, "bob", nil),
			ownedSrv(6, "c1", 3, "carol", nil), ownedSrv(7, "c2", 3, "carol", nil),
		},
	}
	// The narrower list left from a period the BYON machine was shared.
	mr.Set(NetPolicyKey("byon"), `{"c1":[]}`)
	// A stale flag on a node that is (no longer) external is cleared.
	mr.Set(NetPolicyTenancyKey("plat"), "shared")
	pub := NewNetPolicyPublisher(store, rdb)
	pub.RunOnce(context.Background())

	if v, _ := mr.Get(NetPolicyTenancyKey("ext")); v != "shared" {
		t.Errorf("shared external machine not flagged: %q", v)
	}
	if mr.Exists(NetPolicyTenancyKey("plat")) || mr.Exists(NetPolicyTenancyKey("byon")) {
		t.Error("a platform or single-owner machine was flagged")
	}
	raw, _ := mr.Get(NetPolicyKey("byon"))
	var byon map[string][]string
	json.Unmarshal([]byte(raw), &byon)
	if len(byon["c1"]) != 1 || byon["c1"][0] != "c2" || len(byon["c2"]) != 1 || byon["c2"][0] != "c1" {
		t.Errorf("single-owner BYON policy does not let the owner's servers reach each other: %v", byon)
	}
	raw, _ = mr.Get(NetPolicyKey("ext"))
	var ext map[string][]string
	json.Unmarshal([]byte(raw), &ext)
	if len(ext["e1"]) != 0 || len(ext["e2"]) != 1 || ext["e2"][0] != "e3" {
		t.Errorf("external policy does not follow owners: %v", ext)
	}
	raw, _ = mr.Get(NetPolicyKey("plat"))
	var plat map[string][]string
	json.Unmarshal([]byte(raw), &plat)
	if len(plat["p1"]) != 0 || len(plat["p2"]) != 0 {
		t.Errorf("platform policy changed: %v", plat)
	}

	// Bob leaves the external machine: the flag goes.
	store.servers = store.servers[:3]
	store.servers = append(store.servers, ownedSrv(6, "c1", 3, "carol", nil))
	pub.RunOnce(context.Background())
	if mr.Exists(NetPolicyTenancyKey("ext")) {
		t.Error("the flag stayed after the machine went down to one owner")
	}
}

func TestTheTenancyKeyIsTornDownWithTheNode(t *testing.T) {
	found := false
	for _, k := range NodeRedisKeys("tok") {
		found = found || k == NetPolicyTenancyKey("tok")
	}
	if !found {
		t.Fatal("the tenancy flag outlives its node")
	}
}

// M24: a single-owner EXTERNAL machine is outside the platform's fleet too, so
// its owner's servers reach each other exactly as on a single-owner BYON one.
func TestASingleOwnerExternalMachineGetsTheSameOwnerList(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	store := &fakeNetPolicyStore{
		nodes:   []models.Node{{ID: 2, Token: "ext", Tags: "external"}},
		servers: []models.Server{ownedSrv(1, "e1", 2, "alice", nil), ownedSrv(2, "e2", 2, "alice", nil)},
	}
	NewNetPolicyPublisher(store, rdb).RunOnce(context.Background())
	raw, _ := mr.Get(NetPolicyKey("ext"))
	var ext map[string][]string
	json.Unmarshal([]byte(raw), &ext)
	if len(ext["e1"]) != 1 || ext["e1"][0] != "e2" || len(ext["e2"]) != 1 || ext["e2"][0] != "e1" {
		t.Errorf("single-owner external policy does not let the owner's servers reach each other: %v", ext)
	}
	if mr.Exists(NetPolicyTenancyKey("ext")) {
		t.Error("a single-owner machine was flagged shared")
	}
}
