package redisacl

import (
	"sort"
	"strings"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/queue"
)

// commandCats is the category grant every principal here gets: read, write,
// stream, pubsub, connection and transaction, minus dangerous, admin and
// scripting.
//
// SCAN is deliberately NOT in it. It used to be, on the reasoning that KEYS is
// @dangerous and SCAN is not - but Redis does not filter SCAN by the ACL's key
// patterns. It walks the whole keyspace and returns every key NAME, and only a
// command that names a key is checked against the patterns. Measured against
// Valkey 8: a user scoped to a single prefix ran SCAN and got back keys from
// every other prefix; a GET on one of them then answered NOPERM. Values are
// protected, names are not.
//
// That distinction is the whole reason this matters, because the names ARE the
// sensitive part: every server UUID, node token, SFTP account name and link
// token on the platform is a key name, and they are exactly the inputs a
// forged write needs. The log-shipper credential lives in the tenant's own
// Minecraft container, beside plugins the tenant wrote, and it never called
// SCAN once.
var commandCats = []string{
	"+@read", "+@write", "+@stream", "+@pubsub", "+@connection", "+@transaction",
	"-@dangerous", "-@admin", "-@scripting",
}

// nodeCommandCats is commandCats plus SCAN, and ONLY the node agent gets it.
//
// The node genuinely iterates the keyspace - Core discovery, port allocation and
// the disk-full sweep all scan - so removing it would break the agent. Nothing
// else here does: the log-shipper, the node's link sidecar and a route-only
// link make no SCAN call in either repository.
//
// Left as a known, narrowed exposure rather than a solved one: a tenant-owned
// BYON node can still enumerate key names. Closing that needs the three scan
// sites rewritten to read an index instead of walking the keyspace, which is a
// change to the agent and not to this file.
var nodeCommandCats = append(append([]string{}, commandCats...), "+scan")

// globalReadKeys are the shared keys the node accesses (NOT the shipper). The
// ones the node only ever reads are read-only (%R~); dylaris:migration:* stays
// read+write because the node writes its own migration status/meta/endpoint keys.
//
// These are GLOBAL, not node-scoped, so a read+write grant here is a write
// handed to every node in the fleet, tenant-owned BYON nodes included. The three
// beam:bw_* keys are the bandwidth throttle the node is subject to - writable by
// the throttled party is the wrong way round - and routing_mode/file_access_mode
// are Core-authoritative platform switches. node/main.go reads all five with
// rdb.Get and writes none of them, node/beam_throttle.go likewise; verified by
// sweeping node/ for writes to each key.
func globalReadKeys() []string {
	return []string{
		"%R~dylaris:routing_mode", "%R~dylaris:file_access_mode",
		"%R~dylaris:placement:*",
		"%R~beam:bw_limit", "%R~beam:bw_up_internal", "%R~beam:bw_down_internal",
		// Upload-limit config the node enforces on the beam + SFTP + SaveFileContent
		// write paths. Read-only: these are the caps the node is SUBJECT to.
		// Without them the node's quota reads return NOPERM and the shared quota
		// package fails OPEN, silently disabling the node-side size cap and daily
		// quota with nothing failing anywhere.
		//
		// The per-user daily COUNTER is deliberately not here. It used to be, as
		// "~dylaris:beam:daily:*" - read+write, fleet-wide, on a key named after a
		// user - and it is now a per-node selector; see BeamQuotaSelector.
		"%R~beam:max_upload_bytes", "%R~beam:daily_upload_bytes",
		"%R~dylaris:core:*",
	}
}

// BeamQuotaSelector is the ACL selector granting a node the daily upload
// counters of the users whose uploads it actually handles, and nothing else in
// that namespace. Returns "" for a node hosting nobody, so the caller clears the
// selector rather than granting an empty one.
//
// It replaced "~dylaris:beam:daily:*" in globalReadKeys: a fleet-wide read+write
// grant on a key named after a USER. Any node, a tenant-owned BYON machine
// included, could INCRBY a stranger's counter past the configured daily limit
// and lock that account out of uploads for the rest of the UTC day, or reset the
// counter of a user it does host into an unlimited allowance. Nothing in the key
// says who wrote it, and both paths that read it fail OPEN.
//
// A SELECTOR rather than more entries in the root permission, because the two
// halves have different owners and different cadences. The root permission is
// rebuilt from the server list whenever a node connects or reconciles; the
// username set is what sftp_sync already resolves on its own 60s tick. Selectors
// survive "resetkeys", so each writer owns its half without clearing the other's
// - which is what lets this live where the set is already computed instead of
// becoming a second answer to "who is on this node".
//
// The set is exactly right rather than approximately: a beam ticket is minted
// against sftp.access (handlers/beam.go beamAccessCap) and sftp_sync filters the
// same capability, so the users it publishes to a node are the users who can
// upload to it. Two gaps, both degrading to "not counted" and neither to a
// broken upload: a user who gains access mid-tick has no grant for up to 60s,
// and an admin bypasses the resolver so their uploads are not published here at
// all.
//
// Measured against Valkey 8, not assumed: a selector passed as ONE argument is
// accepted, GET/INCRBY/EXPIRE inside it work on the named keys, and DEL, SET,
// DECRBY and GETDEL on the same key all answer NOPERM. The command list is the
// three the shared quota package uses - but it is the KEY PATTERN that contains
// the damage here, not the commands: EXPIRE has to be granted (RecordDailyUsage
// arms the TTL that self-cleans the key), and EXPIRE with a short TTL is a
// reset. That is only ever reachable for the node's own users.
func BeamQuotaSelector(usernames []string) string {
	if len(usernames) == 0 {
		return ""
	}
	// Sorted so a tick that changed nothing sends the identical string, and a
	// diff of two nodes' grants is readable.
	sorted := append([]string(nil), usernames...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("(")
	for _, u := range sorted {
		// Usernames cannot contain ':' or glob metacharacters (validate.Username),
		// which is what makes this interpolation unambiguous.
		b.WriteString("~" + quota.DailyKeyPrefix(u) + "* ")
	}
	b.WriteString("+get +incrby +expire)")
	return b.String()
}

// migrationKeys are the migration grants, split by what the node actually does
// with each sub-namespace instead of the single "~dylaris:migration:*" this
// replaced - which was read+write over the WHOLE namespace, so any node,
// tenant-owned BYON machines included, could rewrite any other server's
// migration state or forge a peer's transfer endpoint.
//
// Scoped by SUB-NAMESPACE rather than per assigned server on purpose: the ACL is
// rebuilt from ListServersByNode on a reconcile tick, so a server migrating IN is
// not yet in the destination node's list at the moment that node has to write its
// status. Per-server grants would make inbound migration fail until the next
// sweep. Sub-namespace scoping keeps the write surface to the three keys the node
// genuinely owns without depending on that timing.
//
// Verified against node/: it writes :status and :meta (migration_commands.go) and
// its own endpoint (migration_server.go), reads a peer's endpoint to pull from it,
// and never touches :orchestration or dylaris:migration:stream - so both stay out.
func migrationKeys(token string) []string {
	return []string{
		// Its own transfer endpoint. Peers' endpoints are readable because a pull
		// migration has to resolve the source node's address.
		"~dylaris:migration:endpoint:" + token,
		"%R~dylaris:migration:endpoint:*",
		// Progress it reports, under a prefix carrying its OWN token.
		//
		// This was "~dylaris:migration:*:status" and ":meta" - read AND write,
		// with the wildcard standing for the server UUID - so every node in the
		// fleet, a machine a customer owns included, could write the migration
		// progress of any server on the platform. Core reads that as authority:
		// "transferred" makes it skip the transfer check, flip node_id to the
		// target and send migrate_cleanup to the source, which deletes the source
		// copy. Nothing in the payload said who wrote it.
		//
		// The wildcard could not simply be narrowed to the servers this node
		// holds, because the TARGET of a move reports "transferred" for a server
		// it does not own yet - which is exactly the timing the old comment here
		// waved at. Naming the key after the reporting node answers both: Redis
		// refuses the cross-node write, and Core reads the key of the node it is
		// actually waiting on. Same fix the backup result channels took below.
		"~" + queue.MigrationNodeKeyPattern(token),
		// Core-authoritative plan. The node reads nothing from it today; read-only
		// so that stays true by construction rather than by convention.
		"%R~dylaris:migration:*:orchestration",
	}
}

// BuildNodeACLRules returns the ACL SETUSER argument list (after the username)
// for the node agent user: own node-scoped keys + assigned server keys + global
// reads + pub/sub channels + command categories.
func BuildNodeACLRules(token, password string, serverUUIDs []string) []interface{} {
	rules := []interface{}{"on", ">" + password, "resetkeys", "resetchannels"}
	rules = append(rules, "~dylaris:node:"+token+":*", "~dylaris:discovery:"+token, "~beam:node-endpoint:"+token)
	// The node's own error stream, read by the panel via
	// services.ErrorStreamServices. Scoped to this node's token exactly like the
	// Link's, so one node cannot write into another's diagnostics.
	//
	// This is the node's ONLY channel that survives a broken control plane: when
	// the gRPC dial fails - a certificate pin that does not match, a Core that
	// refuses the proof - Core learns nothing at all, because the connection it
	// would have learned it from is the one that failed. The node still holds its
	// cached secret and can still reach Redis, so it is the only party in a
	// position to say why it looks offline.
	rules = append(rules, "~dylaris:errors:node:"+token)
	// The per-server storage-path mapping lives under the UN-prefixed
	// node:<token>:server:<uuid>:storage namespace (core handlers/servers_storage.go
	// writes it, node/storage.go reads+writes it) - NOT dylaris:node:*. Without this
	// grant the node gets NOPERM persisting/reading the storage mapping, which the
	// install + reconcile paths hit on every server. Scoped to this node's token.
	rules = append(rules, "~node:"+token+":*")
	// The node reads its own SFTP server list (sftp:node:<nodeName>:user:<user>,
	// nodeName == token) to resolve which server a virtual SFTP path targets.
	// Without this getUserServers gets NOPERM and every SFTP session sees an
	// empty root, so SFTP is dead under mandatory ACL.
	rules = append(rules, "%R~sftp:node:"+token+":*")
	// SFTP password hashes for the users entitled to THIS node, written per node
	// by core/services/sftp_sync.go.
	//
	// This replaced "%R~sftp:auth:*", which was keyed by username alone and so
	// handed every node - a tenant's BYON machine included - the bcrypt hash of
	// every account on the platform, whether or not that user had anything on it.
	rules = append(rules, "%R~"+SFTPAuthKeyPrefix(token)+"*")
	for _, k := range migrationKeys(token) {
		rules = append(rules, k)
	}
	for _, u := range serverUUIDs {
		rules = append(rules, "~dylaris:server:"+u+":*")
	}
	for _, k := range globalReadKeys() {
		rules = append(rules, k)
	}
	// Backup + restore reporting, scoped to THIS node's channel.
	//
	// These were "&dylaris:backup:results" and "&dylaris:backup:restores" - one
	// fleet-wide name each, granted to every node including a tenant-owned BYON
	// machine. Core read the runId straight out of the payload, and Pub/Sub
	// carries no sender identity, so any node could close, complete or resize any
	// run in the fleet: marking a foreign run "success" fires the retention prune,
	// which deletes that job's older archives from storage, and the reported size
	// counts against the owner's backup quota.
	//
	// Per-token, like every other node-scoped name here, so Redis itself refuses a
	// cross-node publish. Core additionally re-derives the owning node from the run
	// and compares it with the token in the channel name.
	rules = append(rules,
		"&"+queue.BackupResultsChannel(token),
		"&"+queue.BackupRestoresChannel(token),
		// Mod-install results, same per-token scoping and for the same reason:
		// the report names a server and a project, and Core acts on it.
		"&"+queue.ModResultsChannel(token),
		// Setup results. A backup import reports what the archive said it was,
		// and Core writes that into the sub-server's install record and mod
		// rows - so a fleet-wide name would let one node restate another
		// server's contents.
		"&"+queue.SetupResultsChannel(token))
	for _, u := range serverUUIDs {
		rules = append(rules, "&dylaris:server:"+u+":stats:live")
	}
	for _, c := range nodeCommandCats {
		rules = append(rules, c)
	}
	return rules
}

// BuildShipperACLRules returns the ACL rules for ONE container's log-shipper
// user: only that server's keys + its stats:live channel. No node-scoped keys,
// no global reads, no :cmds.
//
// One user per SERVER, not per node - the signature takes a single serverUUID
// for that reason. The comment here used to say "the per-node shipper user"
// with a plural "assigned servers' keys", describing the shape this replaced:
// one user holding ~dylaris:server:<u>:* for every server on the machine, where
// dylaris:server:<u>:input is a stdin bridge into a neighbouring tenant's JVM.
func BuildShipperACLRules(password, serverUUID string) []interface{} {
	rules := []interface{}{"on", ">" + password, "resetkeys", "resetchannels"}
	// The six keys the shipper touches, enumerated rather than wildcarded.
	//
	// This credential lives in the MC container's ENVIRONMENT, and that container
	// runs the tenant's own plugins - so everything granted here is granted to
	// code the tenant writes. dylaris:server:<u>:* handed that code write access
	// to the whole namespace, and the namespace holds the keys that ENFORCE
	// things against it:
	//
	//   disk_full     the entire disk-quota guard. The node sets it BEFORE
	//                 gracefulStop, deliberately, because the reconciler ticks
	//                 during those seconds - so the container is still alive and
	//                 could DEL it. desired_state stays "online" on purpose, so
	//                 the marker is the only thing holding the server down, and
	//                 deleting it in a loop lifts the quota entirely.
	//   desired_state the reconciler's start/stop authority. Core re-publishes it
	//                 from the DB every 5s, so forging it is a race rather than a
	//                 bypass - but it is the same grant.
	//   node_busy, live_status, status, reconcile_failed, edge_motd_*, stats:*,
	//                 migration*, proxy_ip:* - none of which the shipper writes.
	//
	// Redis ACL cannot exclude a key from a wildcard (same constraint as the warp
	// principal in the gateway repo), so the only way to withhold those is to name
	// what IS needed. log-shipper is one file; TestShipperACLGrantsEveryKeyItUses
	// keeps this list and that file in step.
	rules = append(rules,
		"~dylaris:server:"+serverUUID+":logs",
		"~dylaris:server:"+serverUUID+":logs:*",
		"~dylaris:server:"+serverUUID+":java-heap",
		"~dylaris:server:"+serverUUID+":input",
		"~dylaris:server:"+serverUUID+":log_filter_rcon",
		"~dylaris:server:"+serverUUID+":stop-requested",
	)
	// The stats:live channel used to be granted here. The shipper contains no
	// Publish and no Subscribe at all - the node's stats collector and Core are
	// the two ends of that channel - so it was a capability handed to tenant code
	// for nothing.
	for _, c := range commandCats {
		rules = append(rules, c)
	}
	return rules
}

// BuildLinkACLRules returns the ACL rules for a node's Link sidecar user: its own
// registration keys (by tunnel token), its discovery + beam-node + beam-endpoint
// keys (by node token), and read-only edge/relay registries. No node-scoped or
// server keys. tunnelToken = DeriveLinkToken(nodeToken, clusterSecret) (the Link's
// AgentSecret, the exact value it writes link:<tunnelToken> under).
func BuildLinkACLRules(password, nodeToken, tunnelToken string) []interface{} {
	rules := []interface{}{"on", ">" + password, "resetkeys", "resetchannels"}
	rules = append(rules,
		"~link:"+tunnelToken,
		"~online_link:"+tunnelToken,
		"~dylaris:errors:link:"+nodeToken,
		// The Link's telemetry stream, named by the same instance its error
		// stream is (link/link.go errLogInstance: the NodeID when there is one).
		//
		// It was missing, and the effect was invisible from either side. The
		// Link publishes to it every tick, Redis answers NOPERM, go-redis
		// returns an ordinary error and the publisher logs to its own stdout;
		// Core's gateway bandwidth consumer scans dylaris:link:*:stats and finds
		// nothing, so the panel's Link bandwidth simply has no data rather than
		// an error. Measured 2026-09-10 in production: NINE link error streams
		// existed, with 4 to 365 entries each, and not one stats stream existed
		// at all - for any link, of any shape.
		//
		// Neither ACL test could see it. The gateway's key sweep checks the
		// Hub's wildcard rules, which DO grant it; the platform twin scans the
		// node's source, and the key is written by the Link, which lives in the
		// other repository. Two builders for one consumer, and each test was
		// looking at the other one.
		"~dylaris:link:"+nodeToken+":stats",
		"~hub:link:discovery:"+nodeToken,
		"~beam:node:"+nodeToken,
		"%R~sys:edges", "%R~edge:registry:*", "%R~edge:cert:fingerprint:*",
		// beam:cert:fingerprint:* is the twin of edge:cert:fingerprint:* above,
		// and it was missing. The Link pins the beam relay's self-signed
		// certificate against it exactly as it pins an edge's, and fails CLOSED
		// on a Redis error - so NOPERM here did not degrade to trust-on-first-use,
		// it made every beam tunnel attempt abort with a TLS alert, forever. The
		// relay logged "link auth read error: remote error: tls: bad certificate"
		// on a loop while the fingerprints matched perfectly, and beam over the
		// relay was simply dead on every node.
		"%R~sys:beams", "%R~beam:registry:*", "%R~beam:cert:fingerprint:*",
		"%R~beam:node-endpoint:"+nodeToken,
	)
	for _, c := range commandCats {
		rules = append(rules, c)
	}
	return rules
}

// SetUserArgs prepends the ACL subcommand + username for rdb.Do(...).
func SetUserArgs(username string, rules []interface{}) []interface{} {
	out := make([]interface{}, 0, len(rules)+3)
	out = append(out, "ACL", "SETUSER", username)
	out = append(out, rules...)
	return out
}

// BuildRouteOnlyLinkACLRules scopes an external route-only link to exactly the keys
// it touches. No hub discovery, no beam: a route-only link has no NodeID and can
// neither publish nor resolve either.
//
// instanceID is the link's ID - which is also its Redis ACL username - and NOT a
// slice of its tunnel token. It used to be tunnelToken[:8], so eight hex
// characters of a live authentication token became a Redis KEY NAME that never
// expires. Key names are readable by anything that can SCAN, so that was one
// tenant's token prefix published to every other tenant's machine. The link ID
// is public by construction: it is the username the link already authenticates
// with, so both sides can name the same stream without either one leaking.
func BuildRouteOnlyLinkACLRules(password, tunnelToken, instanceID string) []interface{} {
	rules := []interface{}{"on", ">" + password, "resetkeys", "resetchannels"}
	rules = append(rules,
		"~link:"+tunnelToken,
		"~online_link:"+tunnelToken,
		"~dylaris:errors:link:"+instanceID,
		// Same stream, same instance, same reason as BuildLinkACLRules above. A
		// route-only link has no NodeID, so errLogInstance falls back to the ACL
		// username - which is what instanceID already is, so both names agree
		// without either side deriving anything new.
		"~dylaris:link:"+instanceID+":stats",
		"%R~sys:edges", "%R~edge:registry:*", "%R~edge:cert:fingerprint:*",
	)
	for _, c := range commandCats {
		rules = append(rules, c)
	}
	return rules
}
