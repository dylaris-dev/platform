// Package beamaccess keeps beam access current: the access-epoch stamps that
// retire outstanding tickets, and the relay a client is sent to.
package beamaccess

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/store"
	beamauth "dylaris-pkg/beam/auth"
)

// A beam ticket is a bearer token that nothing re-reads for its 30 minute life,
// so taking someone's access away only stopped NEW tickets and left an
// outstanding one working. These stamp the server so the node refuses a ticket
// minted before the change.
//
// Every place that changes who may reach a server's files calls one of them.
// There are exactly five such writes in Core and each has exactly one caller -
// CreateInvite, DeleteInvite, UpdateInvitePermissions, UpsertServerGrant,
// DeleteServerGrant - plus the suspension path. A sixth write would need a
// sixth call here, which is the part that can be forgotten; it is named in the
// comment on each store method for that reason.
//
// Best-effort on purpose. The permission change is already committed by the
// time these run, so failing the request now would tell the caller something
// untrue. A failure is logged and the old behaviour - a window until the
// ticket expires - is what remains.

// Stamp marks one server, by its numeric id.
func Stamp(ctx context.Context, st store.Store, rdb *redis.Client, serverID int) {
	if rdb == nil {
		return
	}
	srv, err := st.GetServerByID(serverID)
	if err != nil || srv == nil {
		log.Printf("beam access stamp: could not resolve server %d: %v", serverID, err)
		return
	}
	stampUUID(ctx, rdb, srv.UUID)
	// A grant with inherit on a proxy reaches every same-owner server linked to
	// it (authz resolver), so a change to it changes access to those too. Only
	// the proxy was stamped, and a member removed from it kept an open session
	// on its backends for up to a day.
	if srv.ServerType == "proxy" {
		StampUUIDs(ctx, rdb, ProxyBackendUUIDs(st, srv.ID))
	}
}

// ProxyBackendUUIDs lists the servers linked to a proxy.
func ProxyBackendUUIDs(st store.Store, proxyID int) []string {
	all, err := st.ListAllServers()
	if err != nil {
		log.Printf("beam access stamp: could not list the backends of proxy %d: %v", proxyID, err)
		return nil
	}
	var out []string
	for _, s := range all {
		if s.ProxyID != nil && *s.ProxyID == proxyID {
			out = append(out, s.UUID)
		}
	}
	return out
}

// StampForOwner marks every server an account owns. Used where the
// change is account-wide rather than about one server: an account-scoped grant,
// and suspension.
func StampForOwner(ctx context.Context, st store.Store, rdb *redis.Client, ownerUserID string) {
	if rdb == nil || ownerUserID == "" {
		return
	}
	servers, err := st.ListServersByOwner(ownerUserID)
	if err != nil {
		log.Printf("beam access stamp: could not list servers of %s: %v", ownerUserID, err)
		return
	}
	for _, s := range servers {
		stampUUID(ctx, rdb, s.UUID)
	}
}

// StampForUser marks every server an account can reach - all of
// them for an administrator. Used where the change is about the PERSON: a
// password or second factor reset after the account left its owner's hands,
// the account's deletion, a change of its role. Those paths stamped nothing,
// so a ticket minted before them still opened sessions until it expired.
func StampForUser(ctx context.Context, st store.Store, rdb *redis.Client, userID string) {
	StampUUIDs(ctx, rdb, ReachOf(st, rdb, userID))
}

// ReachOf lists the servers an account can reach now. Taken BEFORE a
// change that shrinks it, and stamped after the change is committed, so no
// ticket minted in between survives.
func ReachOf(st store.Store, rdb *redis.Client, userID string) []string {
	if rdb == nil || st == nil || userID == "" {
		return nil
	}
	u, err := st.GetUserByID(userID)
	if err != nil || u == nil {
		log.Printf("beam access stamp: could not resolve user %s: %v", userID, err)
		return nil
	}
	servers, err := st.ListServersForUser(userID, u.IsAdmin)
	if err != nil {
		log.Printf("beam access stamp: could not list servers of %s: %v", userID, err)
		return nil
	}
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.UUID)
	}
	return out
}

// StampUUIDs marks each server named.
func StampUUIDs(ctx context.Context, rdb *redis.Client, uuids []string) {
	for _, u := range uuids {
		stampUUID(ctx, rdb, u)
	}
}

func stampUUID(ctx context.Context, rdb *redis.Client, uuid string) {
	if uuid == "" {
		return
	}
	// Its own timeout: this runs after the request's real work and must not
	// hold a handler open on a slow Redis.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := beamauth.BumpAccessEpoch(ctx, rdb, uuid); err != nil {
		log.Printf("beam access stamp: %s: %v", uuid, err)
	}
}
