package queue

import "strings"

// Node -> Core setup reporting.
//
// Setup used to be fire-and-forget in both directions: the node installed, wrote
// a status key and told Core nothing else. That is enough while every installer
// is something Core already described - Core picked the loader, so Core knows
// the loader.
//
// Importing a backup archive inverts that. The archive describes ITSELF, and the
// description is inside a tar the node is already unpacking; Core has no copy and
// no way to read one without fetching the whole object back out of the node. So
// the node reports what it found, and Core turns it into the sub-server's install
// record and mod rows. An archive with no description reports none, which is the
// ordinary case for anything written before manifests existed and for an archive
// from somewhere else entirely.
//
// Same shape as the backup and mod channels next door, and for the same reason:
// Pub/Sub carries no sender identity, so the channel name is the only thing a
// message can be attributed by, and a fleet-wide name would let any node - a
// tenant-owned BYON machine included - report on any server's setup. Per token,
// so Redis refuses a cross-node publish outright, and Core additionally
// re-derives the server's node and compares it with the token in the name.
const setupResultsChannelPrefix = "dylaris:setup:results:"

// SetupResultsPattern is what Core PSUBSCRIBEs. A node token is Core-minted and
// carries no Redis glob metacharacters, so the trailing "*" matches exactly one
// token.
const SetupResultsPattern = setupResultsChannelPrefix + "*"

// SetupResultsChannel is the channel a node publishes its setup results on.
// nodeToken is the node's own Core-assigned identity.
func SetupResultsChannel(nodeToken string) string {
	return setupResultsChannelPrefix + nodeToken
}

// NodeTokenFromSetupChannel extracts the publishing node's token from a channel
// name delivered by a pattern subscription. ok is false for anything that is not
// this channel, and for an empty token - both are unattributable and must be
// dropped rather than read as "nothing to check".
func NodeTokenFromSetupChannel(channel string) (token string, ok bool) {
	rest, found := strings.CutPrefix(channel, setupResultsChannelPrefix)
	if !found {
		return "", false
	}
	return rest, rest != ""
}
