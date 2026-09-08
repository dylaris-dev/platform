package services

import (
	"context"
	"log"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// aclReconcilerStore is the narrow data port the ACL reconciler needs, satisfied
// by *store.PostgresStore. GetNodeSecretEnc/SetNodeSecretEnc also let it stand in
// for redisacl's secretStore, so redisacl.LoadNodeSecret (no minting) accepts it.
type aclReconcilerStore interface {
	ListNodes() ([]models.Node, error)
	GetNodeSecretEnc(id int) (string, error)
	SetNodeSecretEnc(id int, enc string) error
	SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error)
	ListServersByNode(nodeID int) ([]models.Server, error)
	ListLinkKitsForACLReconcile(hardSuspendedBefore, overLimitBefore time.Time) ([]store.WarpAPIKey, error)
	// ListLinkKitsForACLTeardown feeds the cleanup sweep: link kits that must
	// NOT have an ACL right now (see reconcileOnce).
	ListLinkKitsForACLTeardown(hardSuspendedBefore, overLimitBefore, revokedAfter time.Time) ([]store.WarpAPIKey, error)
}

// aclTeardownWindow bounds how long a revoked link kit keeps being retried by
// the cleanup sweep: a DELUSER that keeps failing for a full day straight is
// either already gone (a later retry can simply be a no-op) or a deeper
// problem an operator needs to look at, not something to retry forever. The
// owner-hard-suspension arm of the sweep has no such window (see reconcileOnce).
const aclTeardownWindow = 24 * time.Hour

// ACLReconciler re-provisions every paired node's and route-only link's scoped
// Redis ACL users from the DB-stored per-node secret, periodically and on a Redis
// reconnect. It re-creates the users with the SAME derived passwords, so a Valkey
// restart that lost the aclfile self-heals: running services re-auth transparently
// (go-redis) on their next command, no restart. Leader-gated so N Cores do not all
// hammer Redis. Derivation is unchanged; this only re-applies existing users.
type ACLReconciler struct {
	store         aclReconcilerStore
	prov          *redisacl.Provisioner
	redis         *redis.Client
	clusterSecret string
	leader        leader.Election
	interval      time.Duration // full-sweep cadence (~60s)
	probeInterval time.Duration // Redis reachability probe cadence (~10s)
	// graceWindow mirrors cfg.SuspendGrace: a hard-suspended owner's links are
	// excluded from re-provision once suspended_at <= now-graceWindow, matching the
	// billing enforcement cutoff so the reconciler never resurrects a cut link.
	graceWindow time.Duration
}

func NewACLReconciler(st aclReconcilerStore, prov *redisacl.Provisioner, r *redis.Client, clusterSecret string, graceWindow time.Duration) *ACLReconciler {
	return &ACLReconciler{
		store:         st,
		prov:          prov,
		redis:         r,
		clusterSecret: clusterSecret,
		graceWindow:   graceWindow,
		interval:      60 * time.Second,
		probeInterval: 10 * time.Second,
	}
}

func (r *ACLReconciler) SetLeader(l leader.Election) { r.leader = l }

func (r *ACLReconciler) Start(ctx context.Context) {
	if r.prov == nil || r.redis == nil {
		return
	}
	log.Println("ACL Reconciler started (authority-side Redis-ACL re-provision)")
	// One-time boot self-probe so a broken aclfile is caught now, not at the first
	// Valkey restart. Harmless if this replica is not leader (ACL SAVE is idempotent).
	r.prov.SelfProbe(ctx)
	go func() {
		ticker := time.NewTicker(r.probeInterval)
		defer ticker.Stop()
		reachable := true
		var lastReconcile time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if r.leader != nil && !r.leader.IsLeader() {
					// Track reachability so a follower that becomes leader still
					// reconciles on its next tick, but do not provision here.
					reachable = r.redis.Ping(ctx).Err() == nil
					continue
				}
				up := r.redis.Ping(ctx).Err() == nil
				reconnected := up && !reachable
				reachable = up
				if !up {
					continue // Redis down: nothing to provision against
				}
				if reconnected || time.Since(lastReconcile) >= r.interval {
					r.reconcileOnce(ctx)
					lastReconcile = time.Now()
				}
			}
		}
	}()
}

// reconcileOnce re-applies every paired node's and route-only link's ACL users,
// then issues exactly one ACL SAVE. Every step logs and continues on error: one
// bad node must never abort the whole sweep.
func (r *ACLReconciler) reconcileOnce(ctx context.Context) {
	nodes, err := r.store.ListNodes()
	if err != nil {
		log.Printf("acl reconciler: list nodes: %v", err)
		return
	}
	applied := 0
	for _, n := range nodes {
		secret, ok, serr := redisacl.LoadNodeSecret(r.store, r.clusterSecret, n.ID)
		if serr != nil {
			log.Printf("acl reconciler: node %d (%s): load secret: %v", n.ID, tokenPrefix(n.Token), serr)
			continue
		}
		if !ok {
			continue // node not paired yet: nothing to provision
		}
		servers, lerr := r.store.ListServersByNode(n.ID)
		if lerr != nil {
			log.Printf("acl reconciler: node %d (%s): list servers: %v", n.ID, tokenPrefix(n.Token), lerr)
			continue
		}
		uuids := make([]string, 0, len(servers))
		for _, s := range servers {
			uuids = append(uuids, s.UUID)
		}
		tunnelToken := redisacl.LinkTunnelToken(n.Token, r.clusterSecret)
		if aerr := r.prov.EnsureNodeACLNoSave(ctx, n.Token, tunnelToken, secret, uuids); aerr != nil {
			log.Printf("acl reconciler: node %d (%s): ensure ACL: %v", n.ID, tokenPrefix(n.Token), aerr)
			continue
		}
		applied++
	}

	// Route-only external link kits (all tenants) whose owner is NOT hard-suspended.
	// A link kept during grace still routes; once the grace elapses the billing
	// enforcement pass drops its ACL and this query stops resurrecting it. cutoff =
	// now - graceWindow, evaluated once for the whole sweep.
	cutoff := time.Now().Add(-r.graceWindow)
	// The second cutoff is the over-limit one. It is a separate grace with a
	// separate clock, so it cannot be folded into the first: a tenant can be
	// within their payment grace and past their over-limit grace at the same
	// time, and it is the LATTER that has to stop them.
	overCutoff := time.Now().Add(-OverLimitGrace)
	kits, kerr := r.store.ListLinkKitsForACLReconcile(cutoff, overCutoff)
	if kerr != nil {
		log.Printf("acl reconciler: list link kits: %v", kerr)
	} else {
		for _, k := range kits {
			tunnelToken := DeriveLinkToken(k.NodeID, r.clusterSecret)
			if _, _, aerr := r.prov.EnsureRouteOnlyLinkACLNoSave(ctx, r.clusterSecret, k.NodeID, tunnelToken); aerr != nil {
				log.Printf("acl reconciler: link %s: ensure ACL: %v", k.NodeID, aerr)
				continue
			}
			applied++
		}
	}

	// Cleanup sweep: link kits that must NOT have an ACL right now (revoked
	// recently, or owner hard-suspended past grace). Self-heals a DELUSER that
	// failed at revoke/suspend time (a transient Redis blip, see F1): a failed
	// teardown converges here within the next ~60s tick instead of leaving a
	// valid scoped Redis credential live indefinitely. DELUSER is idempotent (no
	// error on an absent user), so re-running this every tick is cheap. This is
	// also the backstop for the LinkBoot/RevokeLinkKit TOCTOU: a link resurrected
	// by an in-flight EnsureRouteOnlyLinkACL that raced a concurrent revoke is
	// caught here on the next tick even if LinkBoot's own fresh re-check
	// (handlers/warp.go) lost the race too.
	teardown, terr := r.store.ListLinkKitsForACLTeardown(cutoff, overCutoff, time.Now().Add(-aclTeardownWindow))
	if terr != nil {
		log.Printf("acl reconciler: list link kits for teardown: %v", terr)
	} else {
		removed := 0
		for _, k := range teardown {
			if r.prov.RemoveRouteOnlyLinkACLNoSave(ctx, k.NodeID) {
				removed++
			}
		}
		// Count what was REMOVED, not what was checked. The hard-suspension arm
		// of this query has no time bound by design, so the checked count stays
		// constant for as long as the tenant is suspended; logging it repeated
		// the same line every tick forever and claimed work that was not done.
		if removed > 0 {
			log.Printf("acl reconciler: swept %d link kit ACL(s) for teardown", removed)
		}
	}

	r.pruneOrphanNodeACLs(ctx, nodes)

	// One persist for the whole sweep (best-effort, loud + throttled on failure).
	r.prov.SaveACL(ctx)
	if applied > 0 {
		log.Printf("acl reconciler: re-applied %d Redis ACL user set(s)", applied)
	}
}

// pruneOrphanNodeACLs deletes ACL users in Core's per-node namespace that no
// longer have a node row. Nothing else does: the loop above only ENSURES ACLs
// for nodes it can see, and DeleteNode's teardown is deliberately best-effort
// (47f6d6b) so a failed DELUSER was never retried. A node row that disappeared
// by any other route - a DB restore predating it, a re-enrolment that minted a
// fresh identity - left three live scoped credentials behind permanently. A
// live stack was observed with three node identities in ACL LIST against one
// database row.
//
// This is the node-side counterpart of the link-kit teardown sweep above, which
// has existed for exactly the same reason.
func (r *ACLReconciler) pruneOrphanNodeACLs(ctx context.Context, nodes []models.Node) {
	// Safety rail. An empty node list is a legitimate state (nobody has paired
	// yet), and in that state every node-* user IS an orphan - but it is also
	// what an unexpected result looks like, and the cost of being wrong is
	// deleting the credentials of the entire fleet at once. Skipping costs one
	// tick: the next node to pair makes the list non-empty and the orphans go
	// then.
	if len(nodes) == 0 {
		return
	}

	res, err := r.redis.Do(ctx, "ACL", "USERS").StringSlice()
	if err != nil {
		log.Printf("acl reconciler: list ACL users for prune: %v", err)
		return
	}
	// The server list per node, because there is one shipper ACL user per server.
	// A lookup failure for one node makes its shipper users look unexpected, and
	// deleting them would cut Redis out from under that node's running
	// containers - so skip the whole sweep rather than prune on a partial view.
	serversByToken := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		servers, lerr := r.store.ListServersByNode(n.ID)
		if lerr != nil {
			logErrf("acl-reconciler", "prune skipped, list servers for node %d (%s): %v",
				n.ID, tokenPrefix(n.Token), lerr)
			return
		}
		uuids := make([]string, 0, len(servers))
		for _, s := range servers {
			uuids = append(uuids, s.UUID)
		}
		serversByToken[n.Token] = uuids
	}
	orphans := redisacl.UnknownNodeACLUsers(res, redisacl.ExpectedNodeACLUsers(serversByToken))
	for _, u := range orphans {
		// Named in full on purpose: this deletes a credential, and an operator
		// needs to be able to tell afterwards which ones went and why.
		log.Printf("acl reconciler: removing orphan node ACL user %q (no matching node row)", u)
		if derr := r.redis.Do(ctx, "ACL", "DELUSER", u).Err(); derr != nil {
			log.Printf("acl reconciler: DELUSER %q: %v", u, derr)
		}
	}
}

// tokenPrefix truncates a node token for logging, matching the hygiene used by the
// gRPC server (grpc/server.go): the full token is the Redis username base and is
// not logged in full.
func tokenPrefix(t string) string {
	if len(t) > 8 {
		return t[:8]
	}
	return t
}
