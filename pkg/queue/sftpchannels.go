package queue

import "strings"

// Node -> Core SFTP audit.
//
// What an SFTP session changed on a server - written, deleted, renamed, made -
// summarised per session and server. The panel's file operations land in the
// server's audit trail; the same operations over SFTP left nothing there.
//
// Per token, like the result channels next door: Pub/Sub carries no sender
// identity, so the channel name is the attribution, Redis refuses a cross-node
// publish, and Core re-derives the server's node and compares it with the token.
const sftpAuditChannelPrefix = "dylaris:sftp:audit:"

// SFTPAuditPattern is what Core PSUBSCRIBEs.
const SFTPAuditPattern = sftpAuditChannelPrefix + "*"

// SFTPAuditChannel is the channel a node publishes its SFTP audit records on.
func SFTPAuditChannel(nodeToken string) string {
	return sftpAuditChannelPrefix + nodeToken
}

// NodeTokenFromSFTPAuditChannel extracts the publishing node's token; ok is
// false for anything unattributable.
func NodeTokenFromSFTPAuditChannel(channel string) (token string, ok bool) {
	rest, found := strings.CutPrefix(channel, sftpAuditChannelPrefix)
	if !found {
		return "", false
	}
	return rest, rest != ""
}

// SFTPAuditRecord is one session's changes to one server.
type SFTPAuditRecord struct {
	ServerUUID string `json:"serverUuid"`
	Username   string `json:"username"`
	RemoteIP   string `json:"remoteIp"`
	Writes     int    `json:"writes"`
	Deletes    int    `json:"deletes"`
	Renames    int    `json:"renames"`
	Mkdirs     int    `json:"mkdirs"`
	// Paths are the first changed paths, relative to the server, capped by the
	// node; Truncated says there were more.
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`
	// Via is the transport: "" for SFTP, "beam" for Beam.
	Via string `json:"via,omitempty"`
}
