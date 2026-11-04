package database

import (
	"database/sql"
	"fmt"
)

// createServerDomainTables owns direct-mode server domains: the DNS accounts
// the platform may write through, the zones they expose, who may use a zone,
// and the domains attached to servers.
func createServerDomainTables(db *sql.DB) error {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS dns_accounts (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			provider TEXT NOT NULL,
			-- Encrypted blob; only DecryptDNSCredentials turns it back.
			enc_credentials TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_error TEXT NOT NULL DEFAULT '',
			last_error_at TIMESTAMPTZ
		)`,
		`CREATE TABLE IF NOT EXISTS dns_zones (
			id SERIAL PRIMARY KEY,
			account_id INTEGER NOT NULL REFERENCES dns_accounts(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			manual BOOLEAN NOT NULL DEFAULT FALSE,
			-- FALSE: the provider's zone listing no longer names it, but a domain
			-- or an owned record still uses it, so the row is kept.
			listed BOOLEAN NOT NULL DEFAULT TRUE,
			UNIQUE (account_id, name)
		)`,
		`ALTER TABLE dns_zones ADD COLUMN IF NOT EXISTS listed BOOLEAN NOT NULL DEFAULT TRUE`,
		// principal is "user:<id>" or "role:<id>".
		`CREATE TABLE IF NOT EXISTS dns_zone_access (
			zone_id INTEGER NOT NULL REFERENCES dns_zones(id) ON DELETE CASCADE,
			principal TEXT NOT NULL,
			PRIMARY KEY (zone_id, principal)
		)`,
		`CREATE TABLE IF NOT EXISTS server_domains (
			id SERIAL PRIMARY KEY,
			server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			fqdn TEXT NOT NULL,
			-- manual | managed
			mode TEXT NOT NULL,
			zone_id INTEGER REFERENCES dns_zones(id) ON DELETE SET NULL,
			label TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'pending',
			problems JSONB NOT NULL DEFAULT '[]',
			observed_at TIMESTAMPTZ,
			created_by TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (server_id, fqdn)
		)`,
		// A managed name points at one server platform-wide. Manual domains may
		// repeat across servers: the platform writes nothing for them.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_server_domains_managed_fqdn
			ON server_domains (fqdn) WHERE mode = 'managed'`,
		// server_domain_id goes NULL when the domain is deleted, and that is the
		// reconciler's signal to remove the record at the provider. value and port
		// are what Core wrote (A: the IP, SRV: target and port): a record at the
		// same name with another value was made or changed by hand and is never
		// touched, so a row without its value would be no proof of ownership.
		`CREATE TABLE IF NOT EXISTS dns_owned_records (
			id SERIAL PRIMARY KEY,
			account_id INTEGER NOT NULL REFERENCES dns_accounts(id) ON DELETE CASCADE,
			zone TEXT NOT NULL,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			value TEXT NOT NULL DEFAULT '',
			port INTEGER NOT NULL DEFAULT 0,
			provider_id TEXT NOT NULL DEFAULT '',
			server_domain_id INTEGER REFERENCES server_domains(id) ON DELETE SET NULL
		)`,
		`ALTER TABLE dns_owned_records ADD COLUMN IF NOT EXISTS value TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dns_owned_records ADD COLUMN IF NOT EXISTS port INTEGER NOT NULL DEFAULT 0`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_dns_owned_records_value
			ON dns_owned_records (account_id, zone, name, type, value, port)`,
		// NULL max_subdomains is no limit, 0 is none.
		`CREATE TABLE IF NOT EXISTS dns_subdomain_limits (
			scope TEXT PRIMARY KEY,
			max_subdomains INTEGER
		)`,
	}
	for _, q := range tables {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("server domain table creation error: %w", err)
		}
	}
	return nil
}
