package services

import (
	"context"
	"fmt"
	"log"
	"strings"

	"dylaris-core/services/redisacl"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// TeardownTenantInfrastructure removes everything an account HOLDS outside its
// own database row: its route-only link kits (durable revoke, scoped Redis ACL
// user, tunnel key) and its protected addresses.
//
// It exists because removing an account did not remove what the account ran,
// and the two paths that remove accounts disagreed about how much of that was
// their problem.
//
// The admin delete endpoint cleaned up ADDRESSES - core_link_routes.owner_id is
// TEXT with no constraint, so nothing cascades and the republisher writes every
// stored row back into Redis every 60 seconds. The auto-delete sweep, which is
// what removes an account in practice, called store.DeleteUser directly and
// cleaned up nothing at all. Its DEFAULT mode is "anonymize", which keeps the
// row: the person is gone, and their link and addresses carry on.
//
// The link kit is the sharper half, and it is invisible to every self-heal the
// platform has. warp_api_keys.owner_id is ON DELETE CASCADE, so deleting the
// account deletes the ROW - and the reconciler's teardown sweep finds work by
// enumerating rows. With the row gone it can never see that the kit's Redis ACL
// user and its tunnel key are still there, both still valid, with no expiry and
// nothing left that refers to them. Measured on the live instance: two link-*
// ACL users against one live kit.
//
// Order is the same one RevokeLinkKitTeardown documents, for the same reason:
// the durable revoke first, so a partial failure cannot undo itself on retry.
// The caller must run this BEFORE removing the account row - afterwards there is
// no owner left to look any of it up by.
//
// A non-nil error means something DURABLE failed and the account must not be
// removed yet. Everything best-effort is logged and carries on, so one
// unreachable route does not strand the rest.
func TeardownTenantInfrastructure(ctx context.Context, st store.Store, gw GatewayProvider, rdb *redis.Client, prov *redisacl.Provisioner, warpPeers WarpPeerDisconnector, userID string) error {
	if st == nil || userID == "" {
		return nil
	}

	// Refuse BEFORE anything is torn down, not after.
	//
	// store.DeleteUser refuses while the account still owns servers, and this
	// runs first - so an operator deleting such an account used to destroy its
	// link kit, its Redis credentials and its protected addresses, and THEN get
	// a 409 telling them to move the servers first. The account survived with
	// everything it ran already gone. Asking the same question up front makes a
	// refused delete change nothing at all, which is what a refusal should mean.
	if n, err := st.CountServersByOwner(userID); err != nil {
		return fmt.Errorf("count servers: %w", err)
	} else if n > 0 {
		return fmt.Errorf("this account still owns %d server(s); move or delete them first", n)
	}

	// INCLUDING the revoked keys, which is the difference between this working
	// and not working on the path that produces most account deletions.
	//
	// Revoking a key blocks the next enrol and leaves the WireGuard peers an
	// established tunnel already has - two paths revoke without removing them on
	// purpose (the suspension cutoff preserves the grace, RevokeLinkKitTeardown
	// leaves the call to its caller). So a tenant who was suspended and deleted
	// weeks later has peers on the leader hanging off keys that are already
	// revoked, and the tenant-facing listing cannot see one of them.
	keys, err := st.ListAllWarpAPIKeysByOwner(userID)
	if err != nil {
		return fmt.Errorf("list the overlay keys of this account: %w", err)
	}

	// Revoke every key BEFORE dropping the peers, so nothing can re-enrol into
	// the gap between the two.
	//
	// The link kits are revoked again inside RevokeLinkKitTeardown below and the
	// second call is a no-op, which is the cheap price of one rule for all of
	// them. The `node-` keys have no other revoker here at all, and that matters
	// most in the DEFAULT delete mode: "anonymize" KEEPS the user row, so nothing
	// cascades, and a live key plus a machine that re-enrols on its own ten
	// minute timer puts the customer's hardware straight back on the overlay.
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue
		}
		if rerr := st.RevokeWarpAPIKeyByNodeID(k.NodeID); rerr != nil {
			return fmt.Errorf("revoke overlay key %s: %w", k.NodeID, rerr)
		}
	}

	// Now the overlay membership itself, and it has to happen while the rows are
	// still here.
	//
	// warp_api_keys.owner_id is ON DELETE CASCADE and warp_peers.api_key_id
	// cascades from there, so removing the account erases the peer ROWS - and a
	// row is the only thing that names a peer. The leader is never told, and its
	// resync rebuilds the peer set from rows that no longer exist, so the
	// WireGuard peer stays configured on the leader with nothing left anywhere
	// that can address it. The account is gone and its machine is still a member
	// of our overlay, permanently, with no surface that can see it.
	//
	// Every key, not just the link kits: a BYON machine holds a `node-` key, and
	// that is the one carrying the tunnel a departing customer's hardware sits on.
	//
	// Unlike RevokeLinkKitTeardown, which leaves this to its caller so the
	// suspension grace is not cut short, there is no grace to preserve here. The
	// account is being deleted.
	if warpPeers != nil {
		peers := 0
		for _, k := range keys {
			peers += warpPeers.DisconnectKeyPeers(ctx, k.ID)
		}
		if peers > 0 {
			log.Printf("tenant teardown for %s: dropped %d overlay peer(s)", userID, peers)
		}
	} else {
		log.Printf("tenant teardown for %s: warp not wired, overlay peers are NOT removed", userID)
	}

	// Link kits next: RevokeLinkKitTeardown also removes the routes that belong
	// to each kit's tunnel, so the sweep below is left with whatever is not tied
	// to a link.
	//
	// Skipped, loudly, when the link plane is not wired rather than silently: on
	// an install with no gateway there are no kits, but a MISSING dependency
	// where there should be one would otherwise look identical to that.
	if gw != nil && rdb != nil && prov != nil {
		for _, k := range keys {
			if !strings.HasPrefix(k.NodeID, "link-") {
				continue
			}
			if _, rerr := RevokeLinkKitTeardown(ctx, st, gw, rdb, prov, k.NodeID, userID); rerr != nil {
				return fmt.Errorf("revoke link kit %s: %w", k.NodeID, rerr)
			}
		}
	} else {
		log.Printf("tenant teardown for %s: link plane not wired, link kits and their credentials are NOT torn down", userID)
	}

	// The nodes the tenant brought. Their machine keeps running after the
	// account goes, and owner_id is ON DELETE SET NULL - so it did not just
	// survive, it BECAME a platform node: still holding its cached
	// .node_secret, still authenticating with its scoped Redis users, and
	// eligible to receive other people's servers. The platform went on
	// trusting hardware belonging to someone who is no longer a customer.
	//
	// Enumerated here rather than after the row goes, because afterwards
	// owner_id is NULL and there is nothing left to match on.
	if rdb != nil {
		nodes, err := st.ListNodesByOwner(userID)
		if err != nil {
			return fmt.Errorf("list the nodes this account brought: %w", err)
		}
		for _, n := range nodes {
			// Credentials first, row second - the reverse of the operator-facing
			// node delete, on purpose. Everything that revokes a node is keyed by
			// its TOKEN, and the token lives in the row: delete the row first and
			// then fail, and nothing can ever name the Redis user again. A node
			// whose access was revoked while its row survives is recoverable; the
			// other way round is not.
			RemoveNodeRedisState(ctx, rdb, prov, n.Token)
			if derr := st.DeleteNode(n.ID); derr != nil {
				return fmt.Errorf("remove node %d (%s): %w", n.ID, n.Name, derr)
			}
		}
	}
	// Whatever addresses are left. A route can outlive the link it was created
	// through, and one created by an admin on the tenant's behalf may never have
	// had a link token at all, so this is keyed on the OWNER rather than on any
	// tunnel.
	if gw == nil {
		return nil
	}
	routes, err := st.ListCoreLinkRoutes()
	if err != nil {
		return fmt.Errorf("list protected addresses: %w", err)
	}
	for _, rt := range routes {
		if rt.OwnerID != userID {
			continue
		}
		if derr := gw.DeleteCoreOwnedRoute(rt.Domain); derr != nil {
			// Durable: a route left behind keeps sending players to a machine
			// whose owner no longer exists, and the republisher will put it back
			// within the minute even if someone clears it by hand.
			return fmt.Errorf("remove protected address %s: %w", rt.Domain, derr)
		}
	}

	return nil
}
