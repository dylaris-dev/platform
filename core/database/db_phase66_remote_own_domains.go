package database

import (
	"database/sql"
	"fmt"
)

// applyRemoteOwnDomains marks the remote_routes rows on a domain of the
// customer's own (not under a provider hoster domain), and indexes the local
// claims by domain for the lookup across users.
//
// Locks: ADD COLUMN with a constant default is catalog-only since Postgres 11,
// a brief ACCESS EXCLUSIVE on remote_routes and no rewrite; existing rows read
// false, which is right, since a route made before this accepted hoster domains
// only. The plain CREATE INDEX holds a SHARE lock on custom_domain_claims (reads
// go on, writes wait) for the build, which on a table of a few rows is short.
func applyRemoteOwnDomains(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE remote_routes ADD COLUMN IF NOT EXISTS custom_domain BOOLEAN NOT NULL DEFAULT FALSE`,
		`CREATE INDEX IF NOT EXISTS idx_custom_domain_claims_domain ON custom_domain_claims (domain)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("remote own domains: %w", err)
		}
	}
	return nil
}
