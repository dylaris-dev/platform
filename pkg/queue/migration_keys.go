package queue

import "fmt"

// Migration progress keys, named by the NODE that reports them.
//
// The node token is in the key because Redis is the only thing that can refuse
// a forged write. These used to be dylaris:migration:<uuid>:status and :meta,
// and the node ACL granted them as dylaris:migration:*:status - read AND write,
// with the wildcard standing for the server UUID. Every node in the fleet,
// including a machine a customer owns, could therefore write the migration
// progress of any server on the platform.
//
// Core reads that key as authority: a status of "transferred" makes it skip the
// transfer check, flip node_id to the target and send migrate_cleanup to the
// source, which deletes the source copy. A tenant looping a forged
// "transferred" for a neighbour's UUID during that neighbour's migration
// destroys their server, and nothing in the payload says who wrote it.
//
// Naming the key after the reporting node is the same fix the backup result
// channels already took (see BackupResultsChannel): the ACL grants one prefix
// per token, so Redis refuses the cross-node write outright, and Core reads the
// key of the node it is actually waiting on rather than a name anyone can claim.
func migrationNodeKey(nodeToken, serverUUID, leaf string) string {
	return fmt.Sprintf("dylaris:migration:node:%s:%s:%s", nodeToken, serverUUID, leaf)
}

// MigrationStatusKey is where nodeToken reports its phase for serverUUID.
func MigrationStatusKey(nodeToken, serverUUID string) string {
	return migrationNodeKey(nodeToken, serverUUID, "status")
}

// MigrationMetaKey is where nodeToken publishes the staged archive's hash and
// size for serverUUID.
func MigrationMetaKey(nodeToken, serverUUID string) string {
	return migrationNodeKey(nodeToken, serverUUID, "meta")
}

// MigrationProgressID names ONE attempt of a move in the progress keys: the
// server's uuid, plus "@attempt" when Core sent one. A node runs migrate_in on
// its own goroutine and keeps going after Core gave up on it (a timeout, a
// cancel, a leader handover), so an earlier attempt's late "transferred" landed
// in the key the next attempt was waiting on - and Core cut over onto the
// earlier, older copy and had the source's current data deleted. Each attempt
// now reports under its own name; a late write lands where nobody reads.
//
// Still under the node's own prefix, so the ACL grant is unchanged. No attempt
// (an older Core) is the bare uuid, exactly the key used before.
func MigrationProgressID(serverUUID, attempt string) string {
	if attempt == "" {
		return serverUUID
	}
	return serverUUID + "@" + attempt
}

// MigrationProgressServer is the server uuid a progress id names.
func MigrationProgressServer(progressID string) string {
	for i := 0; i < len(progressID); i++ {
		if progressID[i] == '@' {
			return progressID[:i]
		}
	}
	return progressID
}

// MigrationNodeKeyPattern is the ACL key pattern granting one node its own
// progress keys and nothing else.
func MigrationNodeKeyPattern(nodeToken string) string {
	return "dylaris:migration:node:" + nodeToken + ":*"
}

// LegacyMigrationStatusKey is the fleet-wide key the node used to write.
//
// Core reads it for ONE purpose: telling "the node has not reported yet" apart
// from "the node is too old to report where I am listening". It is never
// treated as authority - anything may have written it - so a stale or forged
// value can only change an error message.
func LegacyMigrationStatusKey(serverUUID string) string {
	return fmt.Sprintf("dylaris:migration:%s:status", serverUUID)
}
