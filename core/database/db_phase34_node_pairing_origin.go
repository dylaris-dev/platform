package database

import (
	"database/sql"
	"fmt"
)

// applyNodePairingOriginSchema records two facts about a node row that Core
// could not tell apart before.
//
// enrolled_via is HOW the row came to exist. The stale-node sweep deleted every
// operator node that had been offline for a day with no servers, on the belief
// that "the operator can always re-enroll one with the cluster secret". That
// holds only for a node that proved CLUSTER_SECRET to get its row. An external
// node deployed without that secret, whose row an admin created, was locked out
// for good once swept: its cached secret proof names an identity Core no longer
// knows, and the only way back was deleting .node_id and .node_secret on the
// machine by hand. 'cluster_proof' marks the rows the sweep may take. Every row
// written before this column existed holds the empty string and is never swept, because
// nothing can tell an old cluster-minted row from an admin-created one.
//
// last_auth_peer_ip is the TCP source address of the node's last SUCCESSFUL
// authentication, read off the socket. nodes.address is what the node SAID its
// public IP was, which is not something to bind an admission to. The roll-key
// action binds the re-admission it arms to this one. It lives here rather than
// in memory because Core runs as several replicas and a node dials whichever
// one answers.
//
// Additive and idempotent. The nodes table is created at the very start of
// ensureSchema, so a fresh install has both columns after its first boot.
func applyNodePairingOriginSchema(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS enrolled_via TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS last_auth_peer_ip TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("node pairing origin: %s: %w", q, err)
		}
	}
	return nil
}
