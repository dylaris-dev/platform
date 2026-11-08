package database

import (
	"database/sql"
	"fmt"
)

// applyManagedGateway holds the schema for the managed-gateway provider API
// (stage 2). Every change is additive with a NULL default, so an older Core
// replica running during a rollout keeps working.
func applyManagedGateway(db *sql.DB) error {
	// A rolled API key keeps its previous hash valid until prev_key_expires_at.
	// One old key at a time: the next roll overwrites both columns.
	if _, err := db.Exec(`ALTER TABLE api_keys
		ADD COLUMN IF NOT EXISTS prev_key_hash TEXT,
		ADD COLUMN IF NOT EXISTS prev_key_expires_at TIMESTAMPTZ`); err != nil {
		return fmt.Errorf("api_keys roll columns: %w", err)
	}
	// Partial unique index: a previous hash must never collide with another
	// key's lookup, and NULLs (the common case) stay out of the index.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS api_keys_prev_key_hash_idx
		ON api_keys (prev_key_hash) WHERE prev_key_hash IS NOT NULL`); err != nil {
		return fmt.Errorf("api_keys prev_key_hash index: %w", err)
	}
	// Cap on routes on an account own domains, per scope. A table of its own
	// rather than a column on gateway_route_limits: those rows already mean
	// "unlimited routes" when max_routes is NULL, and one row cannot answer two
	// questions with the same NULL. Same scopes, same convention: no row = the
	// scope says nothing, NULL = no limit, 0 = none, n = the cap.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS gateway_custom_domain_limits (
		scope TEXT PRIMARY KEY,
		max_custom_domains INTEGER
	)`); err != nil {
		return fmt.Errorf("gateway_custom_domain_limits: %w", err)
	}
	// An admin takedown of one route-only route. The row stays (the domain
	// stays claimed, the owner still sees it); the republisher and the create
	// path read these columns and keep the route off the edges.
	if _, err := db.Exec(`ALTER TABLE core_link_routes
		ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS suspended_reason TEXT`); err != nil {
		return fmt.Errorf("core_link_routes suspend columns: %w", err)
	}
	return nil
}
