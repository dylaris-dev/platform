package redisacl

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Provisioner applies per-node ACLs using an admin (default-user) Redis client.
type Provisioner struct {
	admin *redis.Client // Core's existing client (default user = admin)
}

func NewProvisioner(admin *redis.Client) *Provisioner { return &Provisioner{admin: admin} }

// EnsureNodeACLNoSave (idempotent) sets the node, shipper, and link users from
// the authoritative server list WITHOUT persisting (no ACL SAVE). secret is the
// node's 32-byte per-node secret. The caller issues a single trailing SaveACL so
// a full reconcile sweep does one temp-file+rename, not one per node.
func (p *Provisioner) EnsureNodeACLNoSave(ctx context.Context, token, tunnelToken string, secret []byte, serverUUIDs []string) error {
	nodePw := NodePassword(secret, token)
	linkPw := LinkPassword(secret, token)

	nodeArgs := SetUserArgs(NodeUsername(token), BuildNodeACLRules(token, nodePw, serverUUIDs))
	if err := p.admin.Do(ctx, nodeArgs...).Err(); err != nil {
		return err
	}
	// One shipper user per SERVER. A single per-node user was granted every
	// server's keys on the machine, and dylaris:server:<u>:input is a stdin
	// bridge into the JVM, so one tenant's container could write to a
	// neighbour's console. Users for servers that left are not deleted here -
	// the reconciler's prune sweep removes whatever ExpectedNodeACLUsers no
	// longer lists, which also covers a server that moved to another node.
	for _, u := range serverUUIDs {
		shipArgs := SetUserArgs(ShipperUsername(token, u), BuildShipperACLRules(ShipperPassword(secret, token, u), u))
		if err := p.admin.Do(ctx, shipArgs...).Err(); err != nil {
			return err
		}
	}
	linkArgs := SetUserArgs(LinkUsername(token), BuildLinkACLRules(linkPw, token, tunnelToken))
	if err := p.admin.Do(ctx, linkArgs...).Err(); err != nil {
		return err
	}
	return nil
}

// SetNodeBeamQuotaGrant replaces the node user's beam-quota selector with one
// naming exactly `usernames` (see BeamQuotaSelector). Called by the SFTP sync,
// which is where that set is resolved.
//
// "clearselectors" runs unconditionally, including for a node left with no
// users: a revoked user whose grant outlives the revocation is the whole shape
// this is meant to prevent, so the empty case has to clear rather than skip.
//
// It touches ONLY selectors, so it cannot disturb a live node's root permission,
// and the node's own reconcile cannot clear this. On a node that has never
// connected this creates the ACL user - verified against Valkey 8: such a user
// is "off" with no password and its AUTH is refused, and the node's first
// connect fills in the rest.
func (p *Provisioner) SetNodeBeamQuotaGrant(ctx context.Context, token string, usernames []string) error {
	args := []interface{}{"ACL", "SETUSER", NodeUsername(token), "clearselectors"}
	if sel := BeamQuotaSelector(usernames); sel != "" {
		args = append(args, sel)
	}
	return p.admin.Do(ctx, args...).Err()
}

// EnsureNodeACL (idempotent) applies the node's scoped users then persists via
// ACL SAVE (best-effort, loud on failure). Safe to call on every connect and
// before every placement.
func (p *Provisioner) EnsureNodeACL(ctx context.Context, token, tunnelToken string, secret []byte, serverUUIDs []string) error {
	if err := p.EnsureNodeACLNoSave(ctx, token, tunnelToken, secret, serverUUIDs); err != nil {
		return err
	}
	// Persist so a Valkey restart (even while Core is offline) keeps these users.
	p.SaveACL(ctx)
	return nil
}

// RemoveNodeACL drops every user belonging to a node (e.g. node deleted).
// Best-effort.
//
// The shipper users are enumerated rather than named: there is one per server
// and this is also called on paths that no longer know the server list. Missing
// one would leave a live credential for a deleted node's container - exactly
// the leak the prune sweep exists to catch, and this is the cheaper place to
// not create it.
func (p *Provisioner) RemoveNodeACL(ctx context.Context, token string) {
	_ = p.admin.Do(ctx, "ACL", "DELUSER", NodeUsername(token)).Err()
	_ = p.admin.Do(ctx, "ACL", "DELUSER", LinkUsername(token)).Err()
	// Pre-split deployments had one bare "node-<token>-shipper"; harmless if absent.
	_ = p.admin.Do(ctx, "ACL", "DELUSER", "node-"+token+"-shipper").Err()
	if users, err := p.admin.Do(ctx, "ACL", "USERS").StringSlice(); err == nil {
		prefix := "node-" + token + "-shipper-"
		for _, u := range users {
			if strings.HasPrefix(u, prefix) {
				_ = p.admin.Do(ctx, "ACL", "DELUSER", u).Err()
			}
		}
	}
	p.SaveACL(ctx)
}

// aclSaveWarnEvery throttles the loud ACL SAVE failure warning so a persistently
// broken aclfile does not flood the log on every provision and reconcile tick.
const aclSaveWarnEvery = 5 * time.Minute

var (
	aclSaveWarnMu   sync.Mutex
	aclSaveWarnLast time.Time
)

// SaveACL persists the in-memory ACL users to the aclfile (ACL SAVE), best-effort.
// On failure it emits a loud, throttled WARN naming the consequence. It never
// returns an error: persistence is best-effort by design and the in-memory ACL
// still enforces; recovery no longer depends on it (the reconciler re-provisions).
func (p *Provisioner) SaveACL(ctx context.Context) {
	err := p.admin.Do(ctx, "ACL", "SAVE").Err()
	if err == nil {
		return
	}
	aclSaveWarnMu.Lock()
	defer aclSaveWarnMu.Unlock()
	if time.Since(aclSaveWarnLast) < aclSaveWarnEvery {
		return
	}
	aclSaveWarnLast = time.Now()
	log.Printf("redisacl: WARNING: ACL SAVE failed: %v. Scoped Redis users are IN-MEMORY ONLY and will be LOST on a Valkey restart until the next reconcile re-provisions them. Ensure Valkey runs with --aclfile on a writable, persisted volume.", err)
}

// SelfProbe runs once at boot to surface a broken ACL-persistence setup early. It
// issues ACL SAVE and, on failure, logs a loud one-time boot warning so an
// operator learns the aclfile is not persisting now, not at the first Valkey
// restart. A passing probe means scoped users survive a restart.
func (p *Provisioner) SelfProbe(ctx context.Context) {
	if err := p.admin.Do(ctx, "ACL", "SAVE").Err(); err != nil {
		log.Printf("redisacl: BOOT WARNING: ACL persistence is NOT working (ACL SAVE failed: %v). Scoped Redis users are IN-MEMORY ONLY; a Valkey restart drops them until the reconciler re-provisions (up to one interval later). Configure Valkey with --aclfile on a writable, persisted volume.", err)
		return
	}
	log.Println("redisacl: ACL persistence verified (ACL SAVE ok)")
}
