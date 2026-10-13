package handlers

import (
	"context"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/redis/go-redis/v9"
)

// A server whose node dies mid-install stays "installing" with nothing anywhere
// saying why.
//
// Core sets that status when it dispatches the job and then waits for the node
// to say otherwise. If the node dies during the install, nothing writes, and the
// row keeps reading "installing" - which on BYON, where the node is somebody's
// home PC, is an ordinary support ticket rather than an exotic one.
//
// THIS DOES NOT INVENT A TIMEOUT, and that is the point. The node already
// maintains a liveness key for exactly as long as it is working
// (holdBusyStatus writes dylaris:server:<uuid>:node_busy on a refresh loop with
// a 30s TTL), so "is anyone installing this" is an answerable question rather
// than a guess about how long an install ought to take. Nobody had to pick a
// number, and no number can be wrong for a slow machine.
//
// Nor does it change the status. "installing" remains TRUE - the install is
// unfinished and will resume when the node returns, because the queue entry is
// still pending and gets re-claimed after ClaimMinIdle. Failing the server here
// would be a lie in the common case (a node rebooting) and would fight the node
// when it came back. What was missing was the explanation, so that is what this
// adds.

// nodeBusyKey mirrors the node's own key name (node/reconciler.go). It is a
// cross-component contract in the same sense the status key is: the node is the
// only writer, Core is a reader, and neither may rename it alone.
func nodeBusyKey(uuid string) string {
	return services.NodeBusyKey(uuid)
}

// installStallReason is what the panel shows. Empty means nothing is wrong.
const installStallReason = "The node running this install went offline. Nothing is lost - it resumes on its own when the node reconnects. If the node is gone for good, reinstall the server."

// annotateStalledInstalls marks servers whose install cannot currently make
// progress, so the panel can say so instead of showing a spinner forever.
//
// ALL of these must hold, and each removes a false positive:
//
//   - the server reads "installing" (nothing else is at stake here)
//   - the node is NOT connected - a connected node either holds the job or will
//     be handed it again by the queue, so there is nothing to report
//   - the node_busy key is absent - present means an install is actively running
//     and the node simply has not finished
//
// The middle condition is what keeps the ordinary dispatch window quiet: between
// Core enqueueing the job and the node picking it up there is no busy key yet,
// and reporting a stall there would fire on every single install.
func annotateStalledInstalls(ctx context.Context, rdb *redis.Client, connected func(nodeID int) bool, servers []models.Server) {
	if connected == nil {
		return
	}
	// Collect first: the overwhelmingly common case is zero installing servers,
	// and that must cost no Redis round trip at all.
	var candidates []int
	for i := range servers {
		if servers[i].Status == "installing" && !connected(servers[i].NodeID) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return
	}
	// Without Redis the busy key cannot be checked. Report the stall anyway: the
	// node is known to be disconnected, which is the load-bearing half, and a
	// stale busy key from a node that is no longer there would only have hidden
	// it.
	if rdb == nil {
		for _, i := range candidates {
			servers[i].InstallStalled = true
			servers[i].InstallStallReason = installStallReason
		}
		return
	}

	pipe := rdb.Pipeline()
	cmds := make([]*redis.IntCmd, len(candidates))
	for n, i := range candidates {
		cmds[n] = pipe.Exists(ctx, nodeBusyKey(servers[i].UUID))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		// A Redis failure must not invent a stall: an unreachable Redis says
		// nothing about whether the node is working.
		return
	}
	for n, i := range candidates {
		if busy, err := cmds[n].Result(); err == nil && busy == 0 {
			servers[i].InstallStalled = true
			servers[i].InstallStallReason = installStallReason
		}
	}
}

// annotateStalledInstallsFor is the call-site wrapper: it pulls the connectivity
// check off AppState so handlers do not each reach for the registry.
func annotateStalledInstallsFor(ctx context.Context, state *AppState, servers []models.Server) {
	if state == nil || state.GRPCRegistry == nil {
		return
	}
	annotateStalledInstalls(ctx, state.Redis, state.GRPCRegistry.IsConnected, servers)
}
