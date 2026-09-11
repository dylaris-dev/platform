package database

import (
	"database/sql"
	"fmt"
)

// applyWarpSchema creates the Warp registry: enrollment API keys, regions, the
// redundant leaders per region, and the enrolled peers.
//
// Multi-hub model (region-as-identity): a REGION owns one WG identity (subnet +
// key derived from CLUSTER_SECRET+region + peer set). The leaders inside a region
// are interchangeable endpoints for that identity; they share the key, subnet and
// peer set and differ only by host:port. A peer therefore pins to a region, not a
// single leader, and its WG IP comes from the region's subnet.
//
// warp_peers.region replaced the old leader_id column (dev clean-slate norm).
func applyWarpSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS warp_api_keys (
		id           SERIAL PRIMARY KEY,
		name         VARCHAR(128) NOT NULL,
		key_hash     VARCHAR(64)  NOT NULL UNIQUE,
		policy       VARCHAR(16)  NOT NULL DEFAULT 'general',
		max_conns    INTEGER      NOT NULL DEFAULT 1,
		on_new_conn  VARCHAR(16)  NOT NULL DEFAULT 'block',
		fixed_wg_ip  TEXT,
		node_id      TEXT,
		region       TEXT,
		revoked_at   TIMESTAMPTZ,
		created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("warp: create warp_api_keys: %w", err)
	}
	// region preference is additive on existing deployments.
	if _, err := db.Exec(`ALTER TABLE warp_api_keys ADD COLUMN IF NOT EXISTS region TEXT`); err != nil {
		return fmt.Errorf("warp: add warp_api_keys.region: %w", err)
	}
	// owner_id binds a key (and the link/route-only kit it backs) to a tenant.
	// NULL for admin-minted keys; set for tenant-minted route-only kits.
	if _, err := db.Exec(`ALTER TABLE warp_api_keys ADD COLUMN IF NOT EXISTS owner_id UUID REFERENCES users(id) ON DELETE CASCADE`); err != nil {
		return fmt.Errorf("warp: add warp_api_keys.owner_id: %w", err)
	}
	// bound_node_id is the node a BYON node key belongs to. The key's own node_id
	// is an OVERLAY identity the panel invents at mint time, and nodes.token is
	// chosen at the gRPC enrol afterwards, so nothing else connects the two - and
	// link-boot needs exactly that connection to answer the key with its node's
	// Link. Set when the node enrols with a token minted for this key, or by the
	// owner for a machine that enrolled before this column existed.
	//
	// SET NULL rather than CASCADE: deleting a machine must not delete the key
	// row, which still counts against the plan and is what the owner revokes.
	// A key whose node is gone reads as unbound, which link-boot answers with a
	// retry rather than with some other node's credentials.
	if _, err := db.Exec(`ALTER TABLE warp_api_keys ADD COLUMN IF NOT EXISTS bound_node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL`); err != nil {
		return fmt.Errorf("warp: add warp_api_keys.bound_node_id: %w", err)
	}
	// One live key per machine. Two would be two doors to the same Link
	// credential, and revoking one would leave the other working. A revoked key
	// drops out, so a machine whose key was replaced can be bound again.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_warp_api_keys_bound_node
		ON warp_api_keys(bound_node_id) WHERE bound_node_id IS NOT NULL AND revoked_at IS NULL`); err != nil {
		return fmt.Errorf("warp: create warp_api_keys bound_node index: %w", err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS warp_regions (
		region     TEXT PRIMARY KEY,
		subnet     TEXT NOT NULL,
		enabled    BOOLEAN NOT NULL DEFAULT TRUE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("warp: create warp_regions: %w", err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS warp_leaders (
		leader_id  TEXT PRIMARY KEY,
		region     TEXT NOT NULL REFERENCES warp_regions(region) ON DELETE CASCADE,
		endpoint   TEXT NOT NULL,
		enabled    BOOLEAN NOT NULL DEFAULT TRUE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("warp: create warp_leaders: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_warp_leaders_region
		ON warp_leaders(region)`); err != nil {
		return fmt.Errorf("warp: create warp_leaders index: %w", err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS warp_peers (
		id           SERIAL PRIMARY KEY,
		api_key_id   INTEGER      NOT NULL REFERENCES warp_api_keys(id) ON DELETE CASCADE,
		pubkey       TEXT         NOT NULL UNIQUE,
		wg_ip        TEXT         NOT NULL UNIQUE,
		region       TEXT         NOT NULL DEFAULT 'leader-01',
		created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("warp: create warp_peers: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_warp_peers_key
		ON warp_peers(api_key_id)`); err != nil {
		return fmt.Errorf("warp: create warp_peers index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_warp_peers_region
		ON warp_peers(region)`); err != nil {
		return fmt.Errorf("warp: create warp_peers region index: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE warp_peers ADD COLUMN IF NOT EXISTS assigned_leader TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("warp: add warp_peers.assigned_leader: %w", err)
	}
	return nil
}
