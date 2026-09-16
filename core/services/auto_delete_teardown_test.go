package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
)

var errTeardownUnreachable = errors.New("hub unreachable")

// autoDeleteFakeStore answers only what processExecutions asks. The embedded nil
// store.Store makes any other call panic loudly rather than pass quietly.
type autoDeleteFakeStore struct {
	store.Store

	due []string

	keys []store.WarpAPIKey
	rows []store.CoreLinkRoute

	revokedKits []string
	deletedIDs  []string
	anonIDs     []string
}

// Asked by the teardown before it destroys anything. No servers and no nodes:
// this test is about the link kit and the addresses.
func (f *autoDeleteFakeStore) CountServersByOwner(string) (int, error)        { return 0, nil }
func (f *autoDeleteFakeStore) ListNodesByOwner(string) ([]models.Node, error) { return nil, nil }

func (f *autoDeleteFakeStore) ListUsersDueForDeletion(time.Time) ([]string, error) {
	return f.due, nil
}
func (f *autoDeleteFakeStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return f.keys, nil
}
func (f *autoDeleteFakeStore) ListAllWarpAPIKeysByOwner(o string) ([]store.WarpAPIKey, error) {
	return f.ListWarpAPIKeysByOwner(o)
}
func (f *autoDeleteFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return f.rows, nil
}
func (f *autoDeleteFakeStore) RevokeWarpAPIKeyByNodeID(nodeID string) error {
	f.revokedKits = append(f.revokedKits, nodeID)
	return nil
}
func (f *autoDeleteFakeStore) DeleteUser(id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}
func (f *autoDeleteFakeStore) AnonymizeUser(id string) error {
	f.anonIDs = append(f.anonIDs, id)
	return nil
}

// insertAuditEvent runs after each execution and writes through the store.
func (f *autoDeleteFakeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

// autoDeleteFakeGateway removes the ROW as well as the routing entry, which is
// what the real DeleteCoreOwnedRoute does (see
// TestDeleteCoreOwnedRoute_RemovesTheRowToo). Modelling that matters here: the
// kit teardown and the owner-keyed sweep both work from the same list, and a
// fake that kept serving a deleted row would show a double removal production
// cannot produce - hiding whether the sweep is really needed for the addresses
// the kit teardown does not cover.
type autoDeleteFakeGateway struct {
	GatewayProvider
	store       *autoDeleteFakeStore
	tunnelToken string
	failFor     map[string]error
	deleted     []string
}

func (g *autoDeleteFakeGateway) LinkToken(string) string { return g.tunnelToken }

func (g *autoDeleteFakeGateway) DeleteCoreOwnedRoute(domain string) error {
	if err := g.failFor[domain]; err != nil {
		return err
	}
	g.deleted = append(g.deleted, domain)
	kept := g.store.rows[:0]
	for _, rt := range g.store.rows {
		if rt.Domain != domain {
			kept = append(kept, rt)
		}
	}
	g.store.rows = kept
	return nil
}

// The sweep that actually removes accounts must remove what those accounts ran.
//
// This is the half that was missing. The admin delete endpoint cleaned up a
// tenant's addresses; this service - which is what removes an account in
// practice, on a daily ticker, with no human present - called store.DeleteUser
// and nothing else. Its DEFAULT mode is "anonymize", which keeps the row: the
// person is gone from the panel while their link kit still authenticates
// against Redis and their addresses still route.
//
// Both modes are asserted, because "anonymize" being the default is exactly why
// testing only the hard-delete branch would have missed this.
func TestAutoDeleteTearsDownWhatTheAccountRan(t *testing.T) {
	const tunnelToken = "tunnel-1"

	for _, mode := range []string{"hard_delete", "anonymize"} {
		t.Run(mode, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			fs := &autoDeleteFakeStore{
				due:  []string{"owner-1"},
				keys: []store.WarpAPIKey{{NodeID: "link-1", OwnerID: "owner-1"}},
				rows: []store.CoreLinkRoute{{Domain: "a.example.com", OwnerID: "owner-1", LinkToken: tunnelToken}},
			}
			gw := &autoDeleteFakeGateway{store: fs, tunnelToken: tunnelToken, failFor: map[string]error{}}

			svc := NewAutoDeleteService(fs, "https://panel.example.test")
			svc.SetLinkACL(gw, rdb, redisacl.NewProvisioner(rdb))
			svc.processExecutions(context.Background(), policySnapshot{Mode: mode})

			// Which identities, not how many calls: the teardown revokes every key
			// of the account up front so nothing can re-enrol while its peers are
			// being dropped, and the kit teardown then revokes the same identity
			// again. In SQL the second one matches no row (revoked_at IS NULL) and
			// changes nothing.
			if len(fs.revokedKits) == 0 {
				t.Errorf("link kit not revoked; its Redis credential and tunnel key outlive the account, and the reconciler's self-heal enumerates ROWS, so once the row cascades away nothing can ever find them")
			}
			for _, id := range fs.revokedKits {
				if id != "link-1" {
					t.Errorf("revoked %q, which is not this account's kit (%v)", id, fs.revokedKits)
				}
			}
			if len(gw.deleted) != 1 || gw.deleted[0] != "a.example.com" {
				t.Errorf("addresses not removed exactly once (%v); the republisher writes every stored row back into Redis every 60 seconds", gw.deleted)
			}
			// And the account itself still goes, in whichever way the policy says.
			if mode == "hard_delete" && len(fs.deletedIDs) != 1 {
				t.Errorf("the account was not deleted: %v", fs.deletedIDs)
			}
			if mode == "anonymize" && len(fs.anonIDs) != 1 {
				t.Errorf("the account was not anonymized: %v", fs.anonIDs)
			}
		})
	}
}

// A teardown that fails must leave the account in place. Removing the only
// record of who owned a live credential is the one direction that cannot be
// undone.
func TestAutoDeleteKeepsTheAccountWhenTeardownFails(t *testing.T) {
	rdb := newQueueTestRedis(t)
	fs := &autoDeleteFakeStore{
		due:  []string{"owner-1"},
		rows: []store.CoreLinkRoute{{Domain: "a.example.com", OwnerID: "owner-1"}},
	}
	gw := &autoDeleteFakeGateway{
		store:       fs,
		tunnelToken: "t",
		failFor:     map[string]error{"a.example.com": errTeardownUnreachable},
	}

	svc := NewAutoDeleteService(fs, "https://panel.example.test")
	svc.SetLinkACL(gw, rdb, redisacl.NewProvisioner(rdb))
	svc.processExecutions(context.Background(), policySnapshot{Mode: "hard_delete"})

	if len(fs.deletedIDs) != 0 {
		t.Errorf("the account was deleted even though its address could not be removed: %v", fs.deletedIDs)
	}
}
