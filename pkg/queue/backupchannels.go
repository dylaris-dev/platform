package queue

import (
	"strconv"
	"strings"
	"time"
)

// Node -> Core backup reporting channels.
//
// These are Pub/Sub, not streams, and Pub/Sub carries NO sender identity: a
// subscriber sees the payload and the channel name, never who published. So the
// channel name is the only thing a message can be attributed by, and that makes
// it the security boundary.
//
// Both channels used to be ONE fleet-wide name each ("dylaris:backup:results",
// "dylaris:backup:restores"), granted to every node by BuildNodeACLRules -
// tenant-owned BYON machines included. Core then took the runId straight out of
// the payload and wrote backup_runs, so any node could close, complete or resize
// ANY run in the fleet: marking a still-running foreign run "success" triggers
// the retention prune, which DELETES that job's older archives from storage, and
// the reported size counts toward the owner's backup quota.
//
// Scoping the channel to the publisher's own node token fixes it in two
// independent layers, which is the same shape every other node-scoped Redis
// name in this system already has (~dylaris:node:<token>:*,
// &dylaris:server:<uuid>:stats:live):
//
//   - Redis refuses the PUBLISH outright, because the node's ACL grants only
//     its own channel.
//   - Core re-derives the node from the run and compares it against the token in
//     the channel name, so a message that somehow arrives on the wrong channel is
//     dropped rather than trusted.
//
// There is no version negotiation and no legacy fallback: Core subscribes to the
// pattern only. A node still publishing to the old fleet-wide name is not heard
// (and is NOPERM'd by its own ACL), so its runs sit "running" until the reaper
// closes them - visible and non-destructive, unlike keeping a spoofable channel
// alive for compatibility. Core and node therefore have to be updated together.
const (
	backupResultsChannelPrefix  = "dylaris:backup:results:"
	backupRestoresChannelPrefix = "dylaris:backup:restores:"

	// BackupResultsPattern / BackupRestoresPattern are what Core PSUBSCRIBEs.
	// A node token is a Core-minted identifier with no Redis glob
	// metacharacters, so the trailing "*" matches exactly one token segment
	// worth of name.
	BackupResultsPattern  = backupResultsChannelPrefix + "*"
	BackupRestoresPattern = backupRestoresChannelPrefix + "*"
)

// BackupResultsChannel is the channel a node publishes its backup-run results
// on. nodeToken is the node's own Core-assigned identity.
func BackupResultsChannel(nodeToken string) string {
	return backupResultsChannelPrefix + nodeToken
}

// BackupRestoresChannel is the channel a node publishes its restore results on.
func BackupRestoresChannel(nodeToken string) string {
	return backupRestoresChannelPrefix + nodeToken
}

// NodeTokenFromBackupChannel extracts the publishing node's token from a channel
// name delivered by a pattern subscription. ok is false for anything that is not
// one of the two backup channels, or that carries an empty token - both of which
// must be treated as unattributable and dropped, never as "no check needed".
func NodeTokenFromBackupChannel(channel string) (token string, ok bool) {
	for _, pfx := range []string{backupResultsChannelPrefix, backupRestoresChannelPrefix} {
		if rest, found := strings.CutPrefix(channel, pfx); found {
			return rest, rest != ""
		}
	}
	return "", false
}

// RestoreResultKey is where a node also keeps a restore's final result, next to
// the PUBLISH. A result published while no Core held the leader lease reached
// nobody, so the reaper read the silence as a lost restore and closed it as
// failed after the world had been replaced. Under the server's own namespace,
// which the node's ACL grants only for the servers it hosts.
func RestoreResultKey(serverUUID string, restoreID int) string {
	return "dylaris:server:" + serverUUID + ":restore_result:" + strconv.Itoa(restoreID)
}

// RestoreResultTTL is how long a node keeps a restore result. Core reads a kept
// result's age as RestoreResultTTL minus the key's remaining TTL, on the Redis
// clock, because a BYON node's own clock can be off by minutes or hours.
const RestoreResultTTL = 24 * time.Hour
