package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
)

// teardownFakeStore answers only what TeardownTenantInfrastructure asks. The
// embedded nil store.Store makes any other call panic loudly.
type teardownFakeStore struct {
	store.Store
	keys        []store.WarpAPIKey
	keysErr     error
	rows        []store.CoreLinkRoute
	revokeCalls []string

	usedLiveOnlyListing bool

	ownedServers  int
	nodes         []models.Node
	deletedNodes  []int
	deleteNodeErr error
}

func (f *teardownFakeStore) CountServersByOwner(string) (int, error)        { return f.ownedServers, nil }
func (f *teardownFakeStore) ListNodesByOwner(string) ([]models.Node, error) { return f.nodes, nil }
func (f *teardownFakeStore) DeleteNode(id int) error {
	if f.deleteNodeErr != nil {
		return f.deleteNodeErr
	}
	f.deletedNodes = append(f.deletedNodes, id)
	return nil
}

func (f *teardownFakeStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	// The teardown must not use this one: it hides revoked keys, and a revoked
	// key is exactly where an orphaned peer hangs. Fail loudly if it does.
	f.usedLiveOnlyListing = true
	return f.keys, f.keysErr
}
func (f *teardownFakeStore) ListAllWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return f.keys, f.keysErr
}
func (f *teardownFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return f.rows, nil
}
func (f *teardownFakeStore) RevokeWarpAPIKeyByNodeID(nodeID string) error {
	f.revokeCalls = append(f.revokeCalls, nodeID)
	return nil
}

// peerRecorder stands in for the warp service: TeardownTenantInfrastructure only
// ever asks it to drop the peers of one key, and what matters is WHICH keys it
// is asked about.
type peerRecorder struct{ keyIDs []int }

func (p *peerRecorder) DisconnectKeyPeers(_ context.Context, keyID int) int {
	p.keyIDs = append(p.keyIDs, keyID)
	return 1
}

// Removing an account has to remove what the account RAN.
//
// warp_api_keys.owner_id is ON DELETE CASCADE, so deleting the account deletes
// the row - and the ACL reconciler's teardown sweep finds work by enumerating
// rows. With the row gone it can never learn that the kit's Redis ACL user and
// tunnel key are still there, valid, with no expiry and nothing referring to
// them. Measured on the live instance: two link-* ACL users against one live
// kit.
func TestTeardownRemovesTheLinkKitAndTheAddresses(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	const tunnelToken = "tunnel-1"

	fs := &teardownFakeStore{
		keys: []store.WarpAPIKey{
			{NodeID: "link-1", OwnerID: "owner-1"},
			// A node enroll key is not a link KIT: the kit revoke must leave it
			// alone (it has no Redis credential of this shape and the cascade
			// removes the row). Its overlay peers are a separate matter and are
			// dropped below.
			{NodeID: "node-abc", OwnerID: "owner-1"},
		},
		rows: []store.CoreLinkRoute{
			{Domain: "a.example.com", OwnerID: "owner-1", LinkToken: tunnelToken},
			// Someone else's address, on the same tunnel. Must survive.
			{Domain: "b.example.com", OwnerID: "owner-2", LinkToken: tunnelToken},
			// Theirs, but never created through a link - an admin can make one
			// on a tenant's behalf. The kit teardown cannot see it, so the
			// owner-keyed sweep has to.
			{Domain: "c.example.com", OwnerID: "owner-1", LinkToken: ""},
		},
	}
	gw := &linkRevokeFakeGateway{tunnelToken: tunnelToken, deleteErrFor: map[string]error{}}

	peers := &peerRecorder{}
	if err := TeardownTenantInfrastructure(ctx, fs, gw, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	// The KIT teardown - ACL user, tunnel key, routes - belongs to link kits only.
	// Every key of the account is REVOKED (see the revoke tests below); this is
	// about which ones get the rest of the treatment.
	if len(gw.tokenedFor) != 1 || gw.tokenedFor[0] != "link-1" {
		t.Errorf("ran the kit teardown for %v, want exactly [link-1] - a node enroll key is not a link kit", gw.tokenedFor)
	}
	deleted := strings.Join(gw.deletedDomains, ",")
	for _, want := range []string{"a.example.com", "c.example.com"} {
		if !strings.Contains(deleted, want) {
			t.Errorf("%s was left routing after its owner was removed; the republisher writes every stored row back to Redis every 60s (deleted: %s)", want, deleted)
		}
	}
	if strings.Contains(deleted, "b.example.com") {
		t.Errorf("removed another owner's address: %s", deleted)
	}
}

// A durable failure must stop the caller from removing the account. Leaving a
// dormant row for another day is recoverable; removing the only record of who
// owned a live credential is not.
func TestTeardownRefusesWhenItCannotSeeWhatTheAccountHolds(t *testing.T) {
	rdb := newQueueTestRedis(t)
	fs := &teardownFakeStore{keysErr: errors.New("db down")}
	gw := &linkRevokeFakeGateway{tunnelToken: "t", deleteErrFor: map[string]error{}}

	peers := &peerRecorder{}
	err := TeardownTenantInfrastructure(context.Background(), fs, gw, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1")
	if err == nil {
		t.Fatal("expected an error when the link kits cannot be listed - the caller would otherwise delete the account and orphan them")
	}
}

// An address that cannot be removed is durable too, for the same reason: it
// keeps sending players to a machine whose owner no longer exists.
func TestTeardownRefusesWhenAnAddressCannotBeRemoved(t *testing.T) {
	rdb := newQueueTestRedis(t)
	fs := &teardownFakeStore{
		rows: []store.CoreLinkRoute{{Domain: "a.example.com", OwnerID: "owner-1"}},
	}
	gw := &linkRevokeFakeGateway{
		tunnelToken:  "t",
		deleteErrFor: map[string]error{"a.example.com": errors.New("hub unreachable")},
	}

	peers := &peerRecorder{}
	err := TeardownTenantInfrastructure(context.Background(), fs, gw, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1")
	if err == nil {
		t.Fatal("expected an error when an address cannot be removed")
	}
}

// A BYON node is a machine on the customer's premises. When the account went,
// nodes.owner_id was ON DELETE SET NULL - so the machine did not merely
// survive, it BECAME a platform node: still holding its cached .node_secret,
// still authenticating with its scoped Redis users, and eligible to receive
// other people's servers. The platform kept trusting hardware belonging to
// someone who is no longer a customer.
func TestTeardownRemovesTheNodesTheTenantBrought(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)

	fs := &teardownFakeStore{nodes: []models.Node{
		{ID: 7, Name: "kitchen-box", Token: "node-token-7"},
		{ID: 9, Name: "attic-box", Token: "node-token-9"},
	}}
	// Seed the keys those nodes own, so their removal is observable.
	for _, tok := range []string{"node-token-7", "node-token-9"} {
		for _, k := range NodeRedisKeys(tok) {
			if err := rdb.Set(ctx, k, "x", 0).Err(); err != nil {
				t.Fatalf("seed %s: %v", k, err)
			}
		}
	}
	// A third node's key, to prove the removal is scoped by token.
	other := NodeRedisKeys("someone-else")[0]
	rdb.Set(ctx, other, "x", 0)

	peers := &peerRecorder{}
	if err := TeardownTenantInfrastructure(ctx, fs, nil, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if len(fs.deletedNodes) != 2 {
		t.Fatalf("deleted %v, want both nodes removed", fs.deletedNodes)
	}
	for _, tok := range []string{"node-token-7", "node-token-9"} {
		for _, k := range NodeRedisKeys(tok) {
			if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
				t.Errorf("key %q outlived the account", k)
			}
		}
	}
	if n, _ := rdb.Exists(ctx, other).Result(); n != 1 {
		t.Error("a key belonging to a different node was removed; the teardown is not scoped")
	}
}

// A node that cannot be removed is a DURABLE failure: the caller's contract is
// that a non-nil error means the account must not be deleted yet. Carrying on
// would leave the account gone and the machine adopted, which is the whole
// thing this exists to prevent.
func TestTeardownStopsWhenANodeCannotBeRemoved(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)

	fs := &teardownFakeStore{
		nodes:         []models.Node{{ID: 7, Name: "kitchen-box", Token: "node-token-7"}},
		deleteNodeErr: errors.New("still has servers on it"),
	}
	peers := &peerRecorder{}
	err := TeardownTenantInfrastructure(ctx, fs, nil, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1")
	if err == nil {
		t.Fatal("teardown reported success while the node stayed")
	}
	if !strings.Contains(err.Error(), "kitchen-box") {
		t.Errorf("error = %q, want it to name the node an operator has to deal with", err)
	}
}

// store.DeleteUser refuses while the account still owns servers, and this runs
// BEFORE it. So an operator deleting such an account destroyed its link kit,
// its Redis credentials and its addresses, and only then got a 409 telling them
// to move the servers first - the account survived with everything it ran
// already gone. A refusal has to change nothing.
func TestTeardownRefusesBeforeItDestroysAnything(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)

	fs := &teardownFakeStore{
		ownedServers: 2,
		keys:         []store.WarpAPIKey{{NodeID: "link-1", OwnerID: "owner-1"}},
		nodes:        []models.Node{{ID: 7, Name: "kitchen-box", Token: "node-token-7"}},
	}
	peers := &peerRecorder{}
	err := TeardownTenantInfrastructure(ctx, fs, nil, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1")
	if err == nil {
		t.Fatal("teardown succeeded for an account that cannot be deleted")
	}
	if !strings.Contains(err.Error(), "2 server") {
		t.Errorf("error = %q, want it to say how many servers are in the way", err)
	}
	if len(fs.revokeCalls) != 0 {
		t.Errorf("revoked %v before refusing; a refused delete must change nothing", fs.revokeCalls)
	}
	if len(fs.deletedNodes) != 0 {
		t.Errorf("deleted nodes %v before refusing", fs.deletedNodes)
	}
}

// The overlay membership, which is the half nothing could ever repair.
//
// warp_api_keys.owner_id cascades and warp_peers.api_key_id cascades from it,
// so removing the account erases the peer ROWS - and a row is the only thing
// that names a peer. The leader is never told, and its resync rebuilds the peer
// set from rows that are gone, so the WireGuard peer stays configured with
// nothing left anywhere that can address it.
//
// Every key, not just the link kits: a BYON machine holds a node- key, and that
// is the one carrying the tunnel the departing customer's hardware sits on.
func TestTeardownDropsTheOverlayPeersOfEveryKeyTheAccountHolds(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &teardownFakeStore{
		keys: []store.WarpAPIKey{
			{ID: 7, NodeID: "link-1", OwnerID: "owner-1"},
			{ID: 9, NodeID: "node-abc", OwnerID: "owner-1"},
		},
	}
	gw := &linkRevokeFakeGateway{tunnelToken: "t", deleteErrFor: map[string]error{}}
	peers := &peerRecorder{}

	if err := TeardownTenantInfrastructure(ctx, fs, gw, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if len(peers.keyIDs) != 2 {
		t.Fatalf("disconnected peers of keys %v, want both 7 and 9 - the node key is the customer's own machine", peers.keyIDs)
	}
	seen := map[int]bool{}
	for _, id := range peers.keyIDs {
		seen[id] = true
	}
	for _, want := range []int{7, 9} {
		if !seen[want] {
			t.Errorf("key %d kept its overlay peers; after the cascade nothing can name them again", want)
		}
	}
}

// Wiring is optional and its absence must not be a panic: an install with no
// overlay has no peers to drop, and the account still has to be removable.
func TestTeardownRunsWithoutTheOverlayWired(t *testing.T) {
	rdb := newQueueTestRedis(t)
	fs := &teardownFakeStore{keys: []store.WarpAPIKey{{ID: 1, NodeID: "link-1", OwnerID: "owner-1"}}}
	gw := &linkRevokeFakeGateway{tunnelToken: "t", deleteErrFor: map[string]error{}}

	if err := TeardownTenantInfrastructure(context.Background(), fs, gw, rdb, redisacl.NewProvisioner(rdb), nil, "owner-1"); err != nil {
		t.Fatalf("teardown without warp: %v", err)
	}
}

// A nil *WarpService reaches the teardown as a NON-nil interface, which is the
// oldest trap in Go and would be a panic in the account delete rather than a
// skipped step. Reachable from any state built without the route wiring.
func TestTeardownSurvivesANilWarpService(t *testing.T) {
	rdb := newQueueTestRedis(t)
	fs := &teardownFakeStore{keys: []store.WarpAPIKey{{ID: 1, NodeID: "link-1", OwnerID: "owner-1"}}}
	gw := &linkRevokeFakeGateway{tunnelToken: "t", deleteErrFor: map[string]error{}}
	var svc *WarpService

	if err := TeardownTenantInfrastructure(context.Background(), fs, gw, rdb, redisacl.NewProvisioner(rdb), svc, "owner-1"); err != nil {
		t.Fatalf("teardown with a nil warp service: %v", err)
	}
}

// The peers that are hardest to see are the ones whose key is already revoked.
//
// Revoking blocks the next enrol and leaves an established tunnel's peers alone,
// and two paths do that deliberately: the suspension cutoff keeps the grace, and
// the shared link teardown leaves the call to its caller. So the ordinary
// sequence - suspend an abusive tenant, delete the account weeks later - reaches
// this function with every peer hanging off a revoked key. Listing only the live
// keys finds nothing to disconnect and the peers outlive the account.
func TestTeardownDropsThePeersOfAlreadyRevokedKeys(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	revoked := time.Now().Add(-72 * time.Hour)
	fs := &teardownFakeStore{
		keys: []store.WarpAPIKey{{ID: 4, NodeID: "link-suspended", OwnerID: "owner-1", RevokedAt: &revoked}},
	}
	gw := &linkRevokeFakeGateway{tunnelToken: "t", deleteErrFor: map[string]error{}}
	peers := &peerRecorder{}

	if err := TeardownTenantInfrastructure(ctx, fs, gw, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if fs.usedLiveOnlyListing {
		t.Error("the teardown asked for the live keys only, which cannot see a suspended tenant's peers at all")
	}
	if len(peers.keyIDs) != 1 || peers.keyIDs[0] != 4 {
		t.Fatalf("disconnected %v, want [4] - a revoked key still holds live WireGuard peers", peers.keyIDs)
	}
}

// Deleting an account does not always delete its row. "anonymize" is the DEFAULT
// mode of the sweep that removes accounts in practice, and it KEEPS the row - so
// nothing cascades, and a key left live is a customer machine that re-enrols on
// its own ten minute timer and is back on the overlay.
func TestTeardownRevokesTheKeysTheCascadeWouldNotTakeAway(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	fs := &teardownFakeStore{
		keys: []store.WarpAPIKey{
			{ID: 1, NodeID: "link-1", OwnerID: "owner-1"},
			{ID: 2, NodeID: "node-abc", OwnerID: "owner-1"},
		},
	}
	gw := &linkRevokeFakeGateway{tunnelToken: "t", deleteErrFor: map[string]error{}}
	peers := &peerRecorder{}

	if err := TeardownTenantInfrastructure(ctx, fs, gw, rdb, redisacl.NewProvisioner(rdb), peers, "owner-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	revoked := map[string]bool{}
	for _, id := range fs.revokeCalls {
		revoked[id] = true
	}
	if !revoked["node-abc"] {
		t.Errorf("revoked %v; the node key was left live, so the machine re-enrols within ten minutes", fs.revokeCalls)
	}
	if !revoked["link-1"] {
		t.Errorf("revoked %v; the link kit was left live", fs.revokeCalls)
	}
}
