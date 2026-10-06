package services

import (
	"context"
	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"dylaris-pkg/fileperms"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SFTPSyncService publishes SFTP auth data and per-node server lists to Redis.
//
// Keys written (both refreshed every 60s, both with a 5min TTL):
//
//	sftp:auth:{nodeToken}:{username}          = bcrypt hash of the panel password
//	sftp:node:{nodeToken}:user:{username}     = JSON [{uuid,name}]
//
// What lands in those keys IS the SFTP authorization decision - the node has no
// second gate behind them, so a server listed here is a server that account can
// read, write and delete files on. That is why the resolver runs below rather
// than the grant tables being trusted as they come.
type SFTPSyncService struct {
	store store.Store
	redis *redis.Client
	authz *authz.Resolver
}

// NewSFTPSyncService takes the same resolver the HTTP routes use. Passing a nil
// one is not supported: this service decides file access, and a nil resolver
// could only mean publishing everything or nothing.
func NewSFTPSyncService(s store.Store, r *redis.Client, az *authz.Resolver) *SFTPSyncService {
	return &SFTPSyncService{store: s, redis: r, authz: az}
}

func (s *SFTPSyncService) Start() {
	log.Println("SFTP Sync Service started")
	s.sync()
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.sync()
		}
	}()
}

// sftpAccessCap is the capability an SFTP session is. The catalog already
// expresses this decision, and the panel's sftp-credentials route already gates
// on it; publishing the credentials has to ask the same question or the two
// disagree - which they did.
//
// It is sftp.access rather than files.read for the same reason beam.go gives
// for its own check: this authorizes the TRANSPORT. Whether a session that is
// allowed to exist should then be read-only is a separate question the node
// would have to enforce per operation, and it does not today.
const sftpAccessCap = "sftp.access"

// sftpServedBy reports whether a node serves SFTP at all, which is a different
// question from whether a given user may use it.
//
// It is the exact rule the panel's credentials route applies
// (handlers/servers_sftp.go): file access "beam" means there is no SFTP, and an
// external node forces beam locally whatever the platform says. Only "beam" is
// off - a mode that was never configured reads as empty and behaves as it
// always has.
//
// This did not exist, so the two surfaces disagreed: the panel answered
// "beam_only" and handed out no host, port or username, while the publisher
// kept every credential an SFTP session needs alive in Redis and the node kept
// accepting them.
func sftpServedBy(node models.Node, fileMode string) bool {
	return !node.IsExternal() && fileMode != "beam"
}

// mayUseSFTP reports whether one candidate row may be published.
//
// Owners short-circuit because the resolver's own owner branch does: keying on
// the row's flag rather than resolving avoids a per-server resolve on every
// tick for the overwhelmingly common case, without changing the answer.
//
// Fails CLOSED. A resolver error drops that pair from this tick rather than
// publishing it, and the keys carry a 5-minute TTL, so a database fault takes
// SFTP away for a few minutes instead of handing out access it could not check.
func (s *SFTPSyncService) mayUseSFTP(a store.SFTPAccess, isAdmin bool) (bool, fileperms.Perms) {
	if a.IsOwner {
		return true, fileperms.Full()
	}
	if s.authz == nil {
		return false, fileperms.Perms{}
	}
	res, err := s.authz.Resolve(authz.Identity{
		UserID:   a.UserID,
		Username: a.Username,
		IsAdmin:  isAdmin,
	}, a.ServerID)
	if err != nil {
		log.Printf("SFTPSync: could not resolve %s on server %d, withholding SFTP: %v", a.Username, a.ServerID, err)
		return false, fileperms.Perms{}
	}
	if !res.HasCap(sftpAccessCap) {
		return false, fileperms.Perms{}
	}
	// The transport is one decision and what may be done through it is another.
	// This used to return here, and the node then allowed every operation - so
	// the built-in Builder role, defined as write-but-not-delete, could remove
	// server.jar over SFTP while HTTP refused the same delete. The resolution is
	// already in hand; the three verbs come out of it.
	//
	// The capability ids are the same strings handlers/file.go passes to
	// getServerUUID for the matching HTTP endpoint - files.read to list or
	// download, files.write to save, create, rename, copy or upload,
	// files.delete to delete. That correspondence is the whole point: a second
	// surface has to ask the same question the first one asks.
	//
	// sftp.access with no file verb at all still gets a session, deliberately.
	// That capability authorizes the TRANSPORT, and TestMayUseSFTP pins it as
	// the thing that decides whether a session exists; making the verbs decide
	// that too would quietly redefine it and take away a login an operator
	// granted on purpose. The session simply cannot do anything, which is what
	// the permissions say.
	return true, fileperms.Perms{
		Read:   res.HasCap("files.read"),
		Write:  res.HasCap("files.write"),
		Delete: res.HasCap("files.delete"),
	}
}

// sftpServerEntry is one server as the node's SFTP server sees it. Perms is
// EMBEDDED rather than nested so the published JSON stays flat
// ({"uuid":..,"name":..,"r":true,..}); an older node that does not know the
// fields ignores them, and a newer node reading an entry published by an older
// Core sees all three false and refuses every operation, which is the safe
// direction and self-heals on the next 60s tick.
type sftpServerEntry struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
	fileperms.Perms
}

// sftpNodeServersKey is the per-node, per-user server list the node's SFTP
// server reads to resolve which server a virtual path targets.
//
// Keyed by the node's TOKEN, never its NAME. Both start out equal - enrollment
// sets nodes.name and nodes.token to the same Core-minted identity - but only
// the token is stable. The panel used to carry a "Node Name" field that renamed
// the row (PATCH /nodes/{id}/config); it is gone, and this key must stay on the
// token anyway - the two can still differ on any row renamed while it existed.
//
// Keying by the name made that rename break SFTP on the node, silently and in
// two ways at once. The node reads this key under the identity Core ASSIGNED it
// (redisacl_bootstrap.go adopts res.AssignedId as nodeID), which is the token;
// and its Redis ACL grants exactly "%R~sftp:node:<token>:*", so even a node that
// somehow knew the new name would get NOPERM on it. The session still
// authenticates - sftp:auth:* is keyed by username and unaffected - so the user
// logs in successfully and sees an EMPTY root, with nothing in any log to say
// why. The token is what every other node-scoped key in the system already uses.
//
// Takes the whole node rather than a string so the choice of field lives here,
// where the reasoning is, instead of at a call site that can pass either one.
func sftpNodeServersKey(node models.Node, username string) string {
	return "sftp:node:" + node.Token + ":user:" + username
}

// sftpNodeKeysKey holds the SSH public keys (authorized_keys lines) an account
// may sign in to this node's SFTP with. Under sftp:node:<token>: so the grant
// the node already has covers it, and a separate key rather than a new shape
// for sftp:auth, which a node one release behind would read as a bcrypt hash.
func sftpNodeKeysKey(node models.Node, username string) string {
	return "sftp:node:" + node.Token + ":keys:" + username
}

// sftpPasswordAllowed reports whether an account may sign in to SFTP with its
// password. SFTP has nowhere to ask for a second factor, so an account that has
// one - or that the platform requires to have one - signs in with a key
// instead. Publishing its hash let the password alone open every file the
// account could reach, which is exactly the theft the second factor is for.
func sftpPasswordAllowed(u models.User, p sftpAuthPolicy) bool {
	if u.Password == "" || u.Is2FAEnabled {
		return false
	}
	return !p.require2FAForAll && !(u.IsAdmin && p.require2FAForAdmins)
}

// sftpLoginAllowed reports whether an account may use SFTP at all: not while
// the platform requires a verified address it has not confirmed - the panel
// refuses that sign-in outright (admins excepted, as there).
func sftpLoginAllowed(u models.User, p sftpAuthPolicy) bool {
	return !(p.emailVerifyRequired && !u.IsAdmin && u.EmailVerifiedAt == nil)
}

// sftpAuthPolicy is the part of the panel's sign-in policy SFTP has to follow.
// Read from the same settings handlers.LoadAuthPolicy reads.
type sftpAuthPolicy struct {
	require2FAForAll, require2FAForAdmins, emailVerifyRequired bool
}

func loadSFTPAuthPolicy(st store.Store) sftpAuthPolicy {
	is := func(key string) bool { v, _ := st.GetSetting(key); return v == "true" }
	return sftpAuthPolicy{
		require2FAForAll:    is("auth.require_2fa_for_all_users"),
		require2FAForAdmins: is("auth.require_2fa_for_admins"),
		emailVerifyRequired: is("auth.email_verify_required"),
	}
}

// pruneStaleAuthKeys removes any sftp:auth:* key whose user is no longer in
// the valid set. SCAN keeps it O(batch) instead of blocking Redis with KEYS.
//
// unknown holds the "sftp:auth:<token>:" prefixes of nodes this tick could not
// build an access list for, and keys under those are left alone. Deleting them
// would turn a transient database error on ONE node's access query into an
// immediate SFTP lockout for every user on that node: the loop below cannot
// tell "this user lost access" from "Core did not get to ask". Withholding the
// refresh is the correct fail-closed response there, and the 5-minute TTL still
// applies - the prune exists to shorten the window after a revocation, not to
// open one after a hiccup.
//
// The per-node server and key lists are pruned the same way. They used to be
// left to their 5-minute TTL, so a revoked grant stayed on the node for up to
// five minutes - and a node that re-reads the list to end a session on
// revocation (sftp_server.go) can only be as quick as this.
//
// keepKeys leaves every published key list alone: this tick could not read the
// keys, and pruning on that would end every key-signed session on the fleet.
func (s *SFTPSyncService) pruneStaleAuthKeys(ctx context.Context, valid map[string]bool, unknown []string, keepKeys bool) {
	for _, pattern := range []string{"sftp:auth:*", "sftp:node:*"} {
		s.pruneStale(ctx, pattern, valid, unknown, keepKeys)
	}
}

func (s *SFTPSyncService) pruneStale(ctx context.Context, pattern string, valid map[string]bool, unknown []string, keepKeys bool) {
	var cursor uint64
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return
		}
		for _, k := range keys {
			if valid[k] || hasAnyPrefix(k, unknown) || (keepKeys && strings.Contains(k, ":keys:")) {
				continue
			}
			s.redis.Del(ctx, k)
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

// hasAnyPrefix reports whether k starts with any of the given prefixes.
func hasAnyPrefix(k string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func (s *SFTPSyncService) sync() {
	ctx := context.Background()

	// 1. Publish auth hashes for all users. Refreshed every tick (60s) with a
	// 5min TTL so the hashes self-expire if this sync ever stops, bounding the
	// exposure window if Redis read access leaks.
	users, err := s.store.ListUsers()
	if err != nil {
		log.Printf("SFTPSync: failed to list users: %v", err)
		return
	}
	// Hash per username, so the per-node publish below can look one up without
	// walking the list again. Nothing is written under a bare "sftp:auth:<user>"
	// any more: that key was readable by EVERY node, so a tenant's own BYON
	// machine held the bcrypt hash of every account on the platform. The hashes
	// now go out per node, in step 2, to the nodes where the user actually has a
	// server - which also means a user with no servers is published nowhere.
	hashByUser := make(map[string]string, len(users))
	policy := loadSFTPAuthPolicy(s.store)
	// Who may use SFTP at all, and the keys of those who may. A key list that
	// cannot be read this tick publishes none: a key that should be gone
	// staying for a tick is worse than one that should be there missing for one.
	blocked := make(map[string]bool)
	keysByUser := make(map[string][]string)
	allKeys, kerr := s.store.ListAllSSHKeys()
	if kerr != nil {
		log.Printf("SFTPSync: could not read SSH keys, publishing none this tick: %v", kerr)
	}
	// The admin flag has to come from here rather than from the access rows: an
	// admin resolves as holding everything, and building the identity without it
	// would resolve them as an ordinary user and drop their own access.
	adminByUser := make(map[string]bool, len(users))
	for _, u := range users {
		adminByUser[u.Username] = u.IsAdmin
		if !sftpLoginAllowed(u, policy) {
			blocked[u.Username] = true
			continue
		}
		if sftpPasswordAllowed(u, policy) {
			hashByUser[u.Username] = u.Password
		}
		if keys := allKeys[u.ID]; len(keys) > 0 {
			keysByUser[u.Username] = keys
		}
	}
	// Owners suspended for non-payment, the same cut-off Beam applies: their
	// servers are not offered over SFTP to them or to anyone they invited.
	suspended := newOwnerSuspension(s.store)
	valid := make(map[string]bool, len(users))
	// Drop auth keys for users that no longer exist (deleted or renamed). The
	// TTL above already bounds this at 5 minutes; the prune is what closes the
	// gap between a deletion in the panel and the moment those credentials stop
	// opening an SFTP session. (An earlier version of this comment claimed the
	// keys carried no TTL and that the prune was the only thing standing between
	// a deleted user and permanent SFTP access - it is not, and reading it that
	// way makes the 5-minute window look like a bug rather than the floor.)
	// 2. Publish per-node, per-user server lists + the auth hashes that node may see
	nodes, err := s.store.ListNodes()
	if err != nil {
		log.Printf("SFTPSync: failed to list nodes: %v", err)
		return
	}

	// What the operator set file access to. A read error leaves it empty, which
	// reads as "not beam" below - the same direction every other mode decision
	// here takes, and the alternative would turn one failed settings read into
	// an SFTP lockout for the whole fleet.
	fileMode, _ := s.store.GetSetting("file_access_mode")

	// Prefixes of nodes whose access list this tick could not read. The prune
	// below skips them instead of treating "no rows" and "no answer" alike.
	var unknown []string
	for _, node := range nodes {
		accesses, err := s.store.GetSFTPAccessByNode(node.ID)
		if err != nil {
			log.Printf("SFTPSync: could not read SFTP access for node %s, leaving its published hashes in place: %v", node.Name, err)
			unknown = append(unknown, redisacl.SFTPAuthKeyPrefix(node.Token), "sftp:node:"+node.Token+":")
			continue
		}

		// Group by username, keeping only what the caller may actually reach.
		//
		// The rows are candidates, not decisions. A grant carries a server role
		// and capability overrides, and this list used to ignore both: any row
		// in server_invites became a full read/write SFTP session. Measured on a
		// live instance - a member invited with every permission off could list
		// the server, read server.properties (which carries the RCON password),
		// write files and delete server.jar, while the same account got 403 on
		// every one of those actions over HTTP. sftp.access already gated the
		// panel's "show me my SFTP credentials" route, so the gate was on the
		// doorbell and not on the door.
		byUser := make(map[string][]sftpServerEntry)
		for _, a := range accesses {
			if blocked[a.Username] || suspended.of(a.ServerID) {
				continue
			}
			ok, perms := s.mayUseSFTP(a, adminByUser[a.Username])
			if !ok {
				continue
			}
			byUser[a.Username] = append(byUser[a.Username],
				sftpServerEntry{UUID: a.ServerUUID, Name: a.ServerName, Perms: perms})
		}

		// The same set decides which daily upload counters this node's Redis
		// credential may touch. Done here rather than in the ACL reconcile because
		// this is where the answer already exists: the reconcile is built from the
		// server list and would have to redo every resolve above to learn it,
		// which is how a second answer to "who is on this node" gets created.
		// See redisacl.BeamQuotaSelector for what the grant replaced.
		//
		// A failed read of the access rows already skipped this node above, so a
		// database fault leaves the previous grant in place instead of revoking
		// every user's counter on a tick that knew nothing.
		//
		// Not on a customer's machine. The counter is per person and
		// platform-wide, the customer holds this credential, and who is "on" the
		// node is theirs to decide: granting a stranger's username was enough to
		// INCRBY that stranger out of uploading anywhere for the day, or to
		// EXPIRE their own count. Uploads to their own disk then go uncounted,
		// which is the lesser loss.
		usernames := make([]string, 0, len(byUser))
		if node.Kind() != models.NodeKindBYON {
			for username := range byUser {
				usernames = append(usernames, username)
			}
		}
		if err := redisacl.NewProvisioner(s.redis).SetNodeBeamQuotaGrant(ctx, node.Token, usernames); err != nil {
			// Loud, because the failure is silent everywhere else: the quota
			// package fails open, so a node left without this grant stops counting
			// uploads rather than refusing them.
			log.Printf("SFTPSync: could not set the beam quota grant for node %s, its uploads may go uncounted: %v", node.Name, err)
		}

		// The beam quota grant above still applies - beam is exactly what a
		// beam-only platform uses - but nothing that OPENS an SFTP session may
		// be published for a node that does not serve one. Leaving `valid`
		// empty for this node is deliberate: the prune at the end then removes
		// the auth hashes already out there, rather than waiting out their TTL.
		//
		// The node refuses the session on its own too (sftpEnabled). This is the
		// other half: a bcrypt hash of every user's password, republished every
		// 60s for a feature that is switched off, is a standing exposure the
		// TTL above exists to bound - not one to keep renewing.
		if !sftpServedBy(node, fileMode) {
			continue
		}

		pipe := s.redis.Pipeline()
		for username, servers := range byUser {
			data, err := json.Marshal(servers)
			if err != nil {
				continue
			}
			listKey := sftpNodeServersKey(node, username)
			pipe.Set(ctx, listKey, data, 5*time.Minute)
			valid[listKey] = true
			if keys, ok := keysByUser[username]; ok {
				if kdata, err := json.Marshal(keys); err == nil {
					keysKey := sftpNodeKeysKey(node, username)
					pipe.Set(ctx, keysKey, kdata, 5*time.Minute)
					valid[keysKey] = true
				}
			}
			// The same TTL as the server list, for the same reason: if this sync
			// stops, the credentials stop opening a session within 5 minutes
			// rather than lingering.
			if hash, ok := hashByUser[username]; ok {
				authKey := redisacl.SFTPAuthKey(node.Token, username)
				pipe.Set(ctx, authKey, hash, 5*time.Minute)
				valid[authKey] = true
			}
		}
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("SFTPSync: failed to write node %s keys: %v", node.Name, err)
		}
	}

	// Drop auth keys that no longer belong: users deleted or renamed, and users
	// whose access to a node was revoked. This runs AFTER the node loop because
	// `valid` is only complete then - pruning first would delete every key the
	// loop had just written. It also clears the old fleet-wide
	// "sftp:auth:<username>" keys from before this was node-scoped, since those
	// can never appear in `valid` again.
	s.pruneStaleAuthKeys(ctx, valid, unknown, kerr != nil)
}

// ownerSuspension answers "is this server's owner suspended for non-payment"
// for one sync tick, reading each server and each owner once.
type ownerSuspension struct {
	st      store.Store
	byOwner map[string]bool
	byID    map[int]bool
}

func newOwnerSuspension(st store.Store) *ownerSuspension {
	return &ownerSuspension{st: st, byOwner: map[string]bool{}, byID: map[int]bool{}}
}

// of fails OPEN on a read error, like the panel's own suspension check: a
// billing read that fails must not take SFTP away from paying customers.
func (o *ownerSuspension) of(serverID int) bool {
	if v, ok := o.byID[serverID]; ok {
		return v
	}
	v := false
	if srv, err := o.st.GetServerByID(serverID); err == nil && srv != nil && srv.OwnerID != "" {
		owner, seen := o.byOwner[srv.OwnerID]
		if !seen {
			b, berr := o.st.GetUserBilling(srv.OwnerID)
			owner = berr == nil && b != nil && b.Status == "suspended"
			o.byOwner[srv.OwnerID] = owner
		}
		v = owner
	}
	o.byID[serverID] = v
	return v
}
