package handlers

import (
	"context"
	"log"
	"time"

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

// stampBeamAccess marks one server, by its numeric id.
func stampBeamAccess(ctx context.Context, state *AppState, serverID int) {
	if state == nil || state.Redis == nil {
		return
	}
	srv, err := state.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		log.Printf("beam access stamp: could not resolve server %d: %v", serverID, err)
		return
	}
	stampBeamAccessUUID(ctx, state, srv.UUID)
}

// stampBeamAccessForOwner marks every server an account owns. Used where the
// change is account-wide rather than about one server: an account-scoped grant,
// and suspension.
func stampBeamAccessForOwner(ctx context.Context, state *AppState, ownerUserID string) {
	if state == nil || state.Redis == nil || ownerUserID == "" {
		return
	}
	servers, err := state.Store.ListServersByOwner(ownerUserID)
	if err != nil {
		log.Printf("beam access stamp: could not list servers of %s: %v", ownerUserID, err)
		return
	}
	for _, s := range servers {
		stampBeamAccessUUID(ctx, state, s.UUID)
	}
}

// stampBeamAccessForUser marks every server an account can reach - all of
// them for an administrator. Used where the change is about the PERSON: a
// password or second factor reset after the account left its owner's hands,
// the account's deletion, a change of its role. Those paths stamped nothing,
// so a ticket minted before them still opened sessions until it expired.
func stampBeamAccessForUser(ctx context.Context, state *AppState, userID string) {
	stampBeamUUIDs(ctx, state, beamReachOf(state, userID))
}

// beamReachOf lists the servers an account can reach now. Taken BEFORE a
// change that shrinks it, and stamped after the change is committed, so no
// ticket minted in between survives.
func beamReachOf(state *AppState, userID string) []string {
	if state == nil || state.Redis == nil || state.Store == nil || userID == "" {
		return nil
	}
	u, err := state.Store.GetUserByID(userID)
	if err != nil || u == nil {
		log.Printf("beam access stamp: could not resolve user %s: %v", userID, err)
		return nil
	}
	servers, err := state.Store.ListServersForUser(userID, u.IsAdmin)
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

func stampBeamUUIDs(ctx context.Context, state *AppState, uuids []string) {
	for _, u := range uuids {
		stampBeamAccessUUID(ctx, state, u)
	}
}

func stampBeamAccessUUID(ctx context.Context, state *AppState, uuid string) {
	if uuid == "" {
		return
	}
	// Its own timeout: this runs after the request's real work and must not
	// hold a handler open on a slow Redis.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := beamauth.BumpAccessEpoch(ctx, state.Redis, uuid); err != nil {
		log.Printf("beam access stamp: %s: %v", uuid, err)
	}
}
