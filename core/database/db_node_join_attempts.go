package database

import (
	"database/sql"
	"fmt"
)

// applyNodeJoinAttemptsSchema records the connections Core REFUSES, so an
// operator can see them and let a machine back in from the panel.
//
// Before this, a refused node produced one log.Printf on Core's stdout and
// nothing else: no row, no audit event, not even the error stream the
// Infrastructure page renders. A node whose secret had diverged from Core's
// retried every thirty seconds forever and was invisible in the panel, which is
// how eu-node-00 sat offline for six hours before anyone looked at the right
// thing. The only way back in was NODE_RECOVERY_TOKEN in the node's own
// environment - on a five-machine Swarm stack, an edit and a redeploy to
// re-admit one host.
//
// Only attempts from an identity that ALREADY has a nodes row are recorded, and
// that bound is the point rather than a shortcut. It is the exact set an
// operator can act on: Core refuses to mint a secret for an identity it does not
// know, so an unknown claimant is not approvable and listing it would only give
// anyone who can reach the port a way to write rows into this table. Unknown
// identities keep the log line they always had.
//
// approved_until and approved_from_ip are the admission itself. It is bounded in
// BOTH time and origin because the identity in a refused attempt is self-claimed:
// anyone who learns a node's id can knock, so an approval that stood forever, or
// for any source address, would be an invitation to whoever knocks next.
func applyNodeJoinAttemptsSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS node_join_attempts (
		node_token          TEXT PRIMARY KEY,
		-- The source address of the TCP connection, read from the socket. The
		-- one field on this row that the caller cannot choose.
		peer_ip             TEXT NOT NULL DEFAULT '',
		-- Everything below is self-reported and shown only so a human can
		-- recognise the machine. None of it is ever treated as fact.
		reported_public_ip  TEXT NOT NULL DEFAULT '',
		reported_private_ips TEXT NOT NULL DEFAULT '',
		hostname            TEXT NOT NULL DEFAULT '',
		cpu_cores           INTEGER NOT NULL DEFAULT 0,
		cpu_model           TEXT NOT NULL DEFAULT '',
		memory_bytes        BIGINT NOT NULL DEFAULT 0,
		release_version     TEXT NOT NULL DEFAULT '',
		reason              TEXT NOT NULL DEFAULT '',
		attempts            INTEGER NOT NULL DEFAULT 1,
		first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		approved_until      TIMESTAMPTZ,
		approved_from_ip    TEXT NOT NULL DEFAULT '',
		approved_by         TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return fmt.Errorf("node join attempts: create table: %w", err)
	}

	// The panel lists the newest first and the row is small, so one index on the
	// sort column is the whole story.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_node_join_attempts_last_seen
		ON node_join_attempts (last_seen_at DESC)`); err != nil {
		return fmt.Errorf("node join attempts: index: %w", err)
	}
	return nil
}
