package services

import (
	"context"
	"fmt"
	"log"

	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// RevokeLinkKitTeardown durably revokes one route-only link kit end to end:
// marks its key revoked (Core then refuses every link-boot and heartbeat from
// it, so nothing re-writes the tunnel key), deletes its tunnel key, and removes
// its Core-owned routes. The durable DB revoke runs FIRST so a partial failure
// below cannot undo itself on retry. Shared by the tenant-facing revoke endpoint and the
// admin force-suspend path (BillingLifecycleService.SuspendNow) so the
// teardown ordering has one source of truth. ownerID scopes the route
// cleanup: a route is only removed when it belongs to this owner AND this
// link's tunnel. Returns the number of routes removed; a non-nil error means
// the durable revoke itself failed (the caller should treat the link as NOT
// torn down and may retry) - every step after that is best-effort and only
// logged, never returned as an error.
func RevokeLinkKitTeardown(ctx context.Context, st store.Store, gw GatewayProvider, rdb *redis.Client, linkID, ownerID string) (removedRoutes int, err error) {
	if derr := st.RevokeWarpAPIKeyByNodeID(linkID); derr != nil {
		return 0, fmt.Errorf("mark revoked: %w", derr)
	}
	tunnelToken := gw.LinkToken(linkID)
	if derr := rdb.Del(ctx, "link:"+tunnelToken, "online_link:"+tunnelToken).Err(); derr != nil {
		log.Printf("revoke link %s: delete tunnel key: %v", linkID, derr)
	}
	// The ROWS, not Redis. This used to enumerate the live routing table, which
	// made the completeness of a revocation depend on the completeness of a
	// cache: a Redis that had lost the entries reported "0 routes removed" and
	// left them stored, and now that the republisher exists they would have come
	// back a minute later, pointing at a link that was just torn down.
	rows, lerr := st.ListCoreLinkRoutes()
	if lerr != nil {
		log.Printf("revoke link %s: list routes: %v", linkID, lerr)
		return 0, nil
	}
	for _, rt := range rows {
		if rt.OwnerID == ownerID && rt.LinkToken == tunnelToken {
			if derr := gw.DeleteCoreOwnedRoute(rt.Domain); derr != nil {
				log.Printf("revoke link %s: delete route %s: %v", linkID, rt.Domain, derr)
				continue
			}
			removedRoutes++
		}
	}
	return removedRoutes, nil
}
