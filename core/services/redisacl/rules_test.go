package redisacl

import (
	"dylaris-pkg/queue"
	"strings"
	"testing"
)

func joinRules(r []interface{}) string {
	parts := make([]string, len(r))
	for i, v := range r {
		parts[i] = v.(string)
	}
	return strings.Join(parts, " ")
}

func TestBuildNodeACLRules(t *testing.T) {
	r := joinRules(BuildNodeACLRules("n1", "pw", []string{"uuid-a"}))
	for _, want := range []string{
		"on", ">pw", "resetkeys", "resetchannels",
		"~dylaris:node:n1:*", "~dylaris:discovery:n1",
		// The un-prefixed node:<token>:* namespace holds the per-server
		// storage-path mapping (node:n1:server:<uuid>:storage); without it the
		// node gets NOPERM on every install + reconcile.
		"~node:n1:*",
		"~dylaris:server:uuid-a:*", "~dylaris:core:*",
		// Node-scoped: the hashes are published per node, so this grant names
		// the token instead of the whole sftp:auth namespace.
		"%R~sftp:auth:n1:*",
		// Upload-limit enforcement needs the node to read the config keys; SFTP
		// needs its own server-list key. Without these the node-side quota fails
		// open and SFTP sees an empty root under mandatory ACL. The per-user
		// daily COUNTER is not here - it is a selector now, see
		// TestBeamQuotaGrantIsPerUser.
		"beam:max_upload_bytes", "beam:daily_upload_bytes",
		"sftp:node:n1:",
		"&dylaris:backup:results:n1", "&dylaris:server:uuid-a:stats:live",
		"+@read", "+@write", "+@stream", "+@pubsub", "-@dangerous", "-scan",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("node rules missing %q in: %s", want, r)
		}
	}
}

// TestNodeBackupChannelsAreNodeScoped pins that a node may publish backup and
// restore results ONLY on its own channel.
//
// Token-exact, not strings.Contains, and that distinction is the whole test:
// "&dylaris:backup:results:n1" CONTAINS "&dylaris:backup:results", so the
// Contains assertion in TestBuildNodeACLRules stayed green across this very
// change and could never have caught the fleet-wide grant it was written
// against.
//
// The fleet-wide grant let any node - a tenant-owned BYON machine included -
// publish a result for ANY run in the fleet. Pub/Sub carries no sender identity,
// so Core had nothing to attribute it by: marking a foreign run "success" fires
// the retention prune, which deletes that job's older archives from storage.
func TestNodeBackupChannelsAreNodeScoped(t *testing.T) {
	tokens := map[string]bool{}
	for _, v := range BuildNodeACLRules("n1", "pw", []string{"uuid-a"}) {
		tokens[v.(string)] = true
	}
	for _, want := range []string{"&dylaris:backup:results:n1", "&dylaris:backup:restores:n1"} {
		if !tokens[want] {
			t.Errorf("node rules missing the per-node channel grant %q", want)
		}
	}
	for _, forbidden := range []string{
		"&dylaris:backup:results", "&dylaris:backup:restores",
		// A wildcard would be the same hole with extra steps.
		"&dylaris:backup:results:*", "&dylaris:backup:restores:*",
		"&dylaris:backup:*",
	} {
		if tokens[forbidden] {
			t.Errorf("node rules grant %q, which lets one node report on every other node's runs", forbidden)
		}
	}
}

// TestNodeGlobalKeysAreReadOnlyWhereTheNodeOnlyReads pins least privilege on the
// GLOBAL (not node-scoped) keys. A read+write grant here is a write handed to
// every node in the fleet, tenant-owned BYON nodes included, and three of these
// are the bandwidth throttle the node itself is subject to.
//
// Token comparison, not strings.Contains: "~beam:bw_limit" is a substring of
// "%R~beam:bw_limit", so a Contains check passes either way and would not have
// caught the grant this test exists for.
func TestNodeGlobalKeysAreReadOnlyWhereTheNodeOnlyReads(t *testing.T) {
	rules := BuildNodeACLRules("n1", "pw", []string{"uuid-a"})
	tokens := make(map[string]bool, len(rules))
	for _, v := range rules {
		tokens[v.(string)] = true
	}

	// node/main.go and node/beam_throttle.go read every one of these with
	// rdb.Get and write none of them.
	readOnly := []string{
		"dylaris:routing_mode",
		"dylaris:file_access_mode",
		"beam:bw_limit",
		"beam:bw_up_internal",
		"beam:bw_down_internal",
	}
	for _, k := range readOnly {
		if tokens["~"+k] {
			t.Errorf("%s is granted read+write; the node only reads it, and it is global", k)
		}
		if !tokens["%R~"+k] {
			t.Errorf("%s is missing its read-only grant (%%R~%s)", k, k)
		}
	}

	// The counterpart: keys the node genuinely writes must stay read+write, so a
	// blanket tightening cannot pass this test either.
	for _, k := range []string{
		// Migration progress, under a prefix carrying this node's own token.
		// It used to be dylaris:migration:*:status and :meta - read+write with
		// the wildcard standing for the SERVER UUID - which is the fleet-wide
		// write this file's whole comment block warns about, and Core reads it
		// as the authority that triggers cutover and source deletion.
		queue.MigrationNodeKeyPattern("n1"),
		// Its own transfer endpoint.
		"dylaris:migration:endpoint:n1",
	} {
		if !tokens["~"+k] {
			t.Errorf("%s must stay read+write - the node writes it", k)
		}
	}
}

// TestNodeMigrationGrantIsNotWholeNamespace pins the split that replaced a
// blanket "~dylaris:migration:*". That grant was read+write over every server's
// migration state, so any node - a tenant-owned BYON machine included - could
// rewrite another server's transfer status or forge a peer's endpoint.
func TestNodeMigrationGrantIsNotWholeNamespace(t *testing.T) {
	tokens := map[string]bool{}
	for _, a := range BuildNodeACLRules("n1", "pw", []string{"uuid-a"}) {
		tokens[a.(string)] = true
	}
	if tokens["~dylaris:migration:*"] {
		t.Error("the blanket read+write grant on dylaris:migration:* is back")
	}
	// Another node's endpoint is readable (a pull migration resolves the source
	// address) but must never be writable, or a node could redirect a transfer.
	if !tokens["%R~dylaris:migration:endpoint:*"] {
		t.Error("peer endpoints must be readable for pull migration")
	}
	if tokens["~dylaris:migration:endpoint:*"] {
		t.Error("peer endpoints must NOT be writable - a node could forge a transfer source")
	}
	// Core owns the plan; the node reads nothing from it today and must not write it.
	if tokens["~dylaris:migration:*:orchestration"] {
		t.Error("orchestration is Core-authoritative and must not be writable by a node")
	}
	// And the progress keys must not be writable across the fleet either. This
	// was the surviving half of the same defect: the namespace was split, but
	// :status and :meta kept the server UUID as the wildcard, so every node
	// could still write every server's migration phase.
	if tokens["~dylaris:migration:*:status"] || tokens["~dylaris:migration:*:meta"] {
		t.Error("the fleet-wide migration progress write is back; a node can forge another server's phase")
	}
}

// TestNodeSFTPAuthGrantIsNodeScoped pins that a node can only read the SFTP
// password hashes Core published FOR IT. The previous "%R~sftp:auth:*" handed
// every node the bcrypt hash of every account on the platform.
func TestNodeSFTPAuthGrantIsNodeScoped(t *testing.T) {
	tokens := map[string]bool{}
	for _, a := range BuildNodeACLRules("n1", "pw", nil) {
		tokens[a.(string)] = true
	}
	if tokens["%R~sftp:auth:*"] || tokens["~sftp:auth:*"] {
		t.Error("the fleet-wide sftp:auth grant is back")
	}
	if !tokens["%R~"+SFTPAuthKeyPrefix("n1")+"*"] {
		t.Errorf("missing the node-scoped sftp auth grant; got: %v", tokens)
	}
}

// The two copies of this key derivation live in different Go modules and cannot
// import each other, so the shape is pinned here as well as on the node side.
func TestSFTPAuthKeyShape(t *testing.T) {
	if got := SFTPAuthKey("n1", "alice"); got != "sftp:auth:n1:alice" {
		t.Errorf("SFTPAuthKey = %q, want %q", got, "sftp:auth:n1:alice")
	}
	if got := SFTPAuthKeyPrefix("n1"); got != "sftp:auth:n1:" {
		t.Errorf("SFTPAuthKeyPrefix = %q, want %q", got, "sftp:auth:n1:")
	}
}

func TestBuildShipperACLRulesIsNarrow(t *testing.T) {
	r := joinRules(BuildShipperACLRules("pw", "uuid-a"))
	for _, needed := range []string{
		"~dylaris:server:uuid-a:logs",
		"~dylaris:server:uuid-a:logs:*",
		"~dylaris:server:uuid-a:java-heap",
		"~dylaris:server:uuid-a:input",
		"~dylaris:server:uuid-a:log_filter_rcon",
		"~dylaris:server:uuid-a:stop-requested",
	} {
		if !strings.Contains(r, needed) {
			t.Errorf("shipper must be granted %q: %s", needed, r)
		}
	}
	// The whole point of enumerating them. This credential is in the tenant's
	// own container, and the namespace holds the keys that enforce limits
	// AGAINST that tenant - disk_full is the entire disk-quota guard.
	if strings.Contains(r, "~dylaris:server:uuid-a:*") {
		t.Errorf("shipper is back on a namespace wildcard, which grants tenant code disk_full and desired_state: %s", r)
	}
	// It contains no Publish and no Subscribe, so a channel is a capability for
	// nothing.
	if strings.Contains(r, "&") {
		t.Errorf("shipper granted a pub/sub channel it never uses: %s", r)
	}
	// ONE server, never the node's whole set. dylaris:server:<u>:input is a stdin
	// bridge into the JVM, so a second server's keys here would let one tenant's
	// container write into a neighbour's console.
	if strings.Contains(r, "uuid-b") {
		t.Errorf("shipper rules reach a second server: %s", r)
	}
	for _, forbidden := range []string{"dylaris:node:", "dylaris:core:", "sftp:auth", ":cmds"} {
		if strings.Contains(r, forbidden) {
			t.Errorf("shipper rules must NOT contain %q: %s", forbidden, r)
		}
	}
}

// The Link pins BOTH the edge and the beam relay against a stored certificate
// fingerprint, and fails CLOSED when the lookup errors. A missing grant is
// therefore not a degraded pin - it is a permanent connection failure that
// looks like a certificate problem: the relay logs "tls: bad certificate" on a
// loop while the fingerprints match perfectly.
//
// beam:cert:fingerprint:* was missing while its edge twin was present, so beam
// over the relay could not work on any node. Table-driven so the next
// registry/fingerprint pair cannot be half-added either.
func TestLinkCanPinEverythingItDials(t *testing.T) {
	tokens := map[string]bool{}
	for _, a := range BuildLinkACLRules("pw", "n1", "tunnel-token") {
		tokens[a.(string)] = true
	}
	for _, pair := range []struct{ registry, fingerprint string }{
		{"%R~edge:registry:*", "%R~edge:cert:fingerprint:*"},
		{"%R~beam:registry:*", "%R~beam:cert:fingerprint:*"},
	} {
		if !tokens[pair.registry] {
			t.Errorf("missing %q", pair.registry)
		}
		if !tokens[pair.fingerprint] {
			t.Errorf("the Link may discover via %q but cannot read %q, so it can never "+
				"complete a pinned TLS handshake there", pair.registry, pair.fingerprint)
		}
	}
}
