package database

import (
	"database/sql"
	"fmt"
	"log"
)

func createUsersTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		return fmt.Errorf("create pgcrypto extension: %w", err)
	}
	query := `CREATE TABLE IF NOT EXISTS users (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		username TEXT NOT NULL UNIQUE,
		password TEXT NOT NULL,
		minecraft_username TEXT,
		email TEXT,
		is_admin BOOLEAN DEFAULT FALSE,
		is_2fa_enabled BOOLEAN DEFAULT FALSE,
		permissions TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		last_username_change TIMESTAMPTZ
	)`
	_, err := db.Exec(query)
	return err
}

func createModulesTable(db *sql.DB) error {
	// access_role gates which user role can see/use the module:
	//   "all"   — both admins and regular users (default for user-facing tabs)
	//   "admin" — admin-only (Admin, Infrastructure, Settings adjacent tabs)
	// Servers is hard-coded to "all" by seed and cannot be changed.
	query := `CREATE TABLE IF NOT EXISTS modules (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		icon TEXT NOT NULL,
		url TEXT,
		is_enabled BOOLEAN DEFAULT TRUE,
		is_system BOOLEAN DEFAULT FALSE,
		position INTEGER DEFAULT 99,
		access_role TEXT DEFAULT 'all'
	)`
	_, err := db.Exec(query)
	return err
}

func createNodesTable(db *sql.DB) error {
	// Extended with Link settings
	query := `CREATE TABLE IF NOT EXISTS nodes (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		address TEXT NOT NULL,
		token TEXT NOT NULL,
		status TEXT DEFAULT 'offline',
		is_local BOOLEAN DEFAULT FALSE,
		tags TEXT,
		link_enabled BOOLEAN DEFAULT FALSE,
		link_instances INTEGER DEFAULT 1,
		link_secret TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`
	_, err := db.Exec(query)
	return err
}

func createServersTable(db *sql.DB) error {
	query := `CREATE TABLE IF NOT EXISTS servers (
		id SERIAL PRIMARY KEY,
		uuid TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		node_id INTEGER NOT NULL REFERENCES nodes(id),
		owner_id UUID NOT NULL REFERENCES users(id),
		game_image TEXT,
		port INTEGER,
		memory INTEGER,
		start_command TEXT,
		status TEXT DEFAULT 'offline',
		is_fixed BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`
	_, err := db.Exec(query)
	return err
}

func createSettingsTable(db *sql.DB) error {
	query := `CREATE TABLE IF NOT EXISTS settings (
		key   VARCHAR(100) PRIMARY KEY,
		value TEXT
	)`
	_, err := db.Exec(query)
	return err
}

func createServerInvitesTable(db *sql.DB) error {
	query := `CREATE TABLE IF NOT EXISTS server_invites (
		id SERIAL PRIMARY KEY,
		server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		permissions JSONB DEFAULT '{}',
		-- Nullable + SET NULL so a grant outlives the account that issued it;
		-- see applyInviteAttributionNullable for why CASCADE would be wrong.
		invited_by UUID REFERENCES users(id) ON DELETE SET NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(server_id, user_id)
	)`
	_, err := db.Exec(query)
	return err
}

func migrateSchema(db *sql.DB) error {
	cols := []struct{ table, col, def string }{
		{"users", "email", "TEXT"},
		{"users", "is_2fa_enabled", "BOOLEAN DEFAULT FALSE"},
		{"users", "totp_secret", "TEXT DEFAULT ''"},
		{"users", "totp_backup_codes", "JSONB DEFAULT '[]'"},
		{"users", "permissions", "TEXT"},
		{"nodes", "tags", "TEXT"},
		{"nodes", "link_enabled", "BOOLEAN DEFAULT FALSE"},
		{"nodes", "link_instances", "INTEGER DEFAULT 1"},
		{"nodes", "link_secret", "TEXT"},
		{"servers", "start_command", "TEXT"},
		{"servers", "is_fixed", "BOOLEAN DEFAULT TRUE"},
		{"servers", "active_sub_server", "TEXT DEFAULT ''"},
		{"servers", "extra_jvm_flags", "TEXT DEFAULT ''"},
		{"servers", "ports", "JSONB DEFAULT '[]'"},
		{"servers", "cpu_limit", "REAL DEFAULT 0"},
		{"nodes", "public_ip", "TEXT DEFAULT ''"},
		{"nodes", "private_ips", "JSONB DEFAULT '[]'"},
		{"servers", "installer_type", "TEXT DEFAULT ''"},
		{"servers", "minecraft_version", "TEXT DEFAULT ''"},
		{"servers", "build_number", "TEXT DEFAULT ''"},
		{"servers", "disk_limit", "BIGINT DEFAULT 0"},
		{"nodes", "cpuset_cpus", "TEXT DEFAULT ''"},
		{"servers", "desired_state", "TEXT DEFAULT 'stopped'"},
		{"servers", "server_type", "TEXT DEFAULT 'game'"},
		{"servers", "proxy_id", "INTEGER REFERENCES servers(id) ON DELETE SET NULL"},
		{"nodes", "link_image", "TEXT DEFAULT ''"},
		{"nodes", "last_seen_at", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"},
		{"servers", "host_port", "INT DEFAULT 0"},
		{"servers", "container_port", "INT DEFAULT 25565"},
		{"modules", "access_role", "TEXT DEFAULT 'all'"},
		// Auto-move: when true, the rebalance loop is allowed to migrate this
		// server to another node when the current node is overloaded.
		// Migration only happens while the server is stopped/idle.
		{"servers", "auto_move", "BOOLEAN DEFAULT FALSE"},
		// Per-node placement: overcommit ratios + observed capacity floor.
		// 1.0 = no overcommit, 1.5 = allow 50% over physical capacity.
		// total_* are last-known physical totals reported by the node's
		// heartbeat — cached so the scheduler does not need a live ping.
		{"nodes", "cpu_overcommit_ratio", "REAL DEFAULT 1.0"},
		{"nodes", "ram_overcommit_ratio", "REAL DEFAULT 1.0"},
		{"nodes", "total_cpu", "REAL DEFAULT 0"},
		{"nodes", "total_ram_mb", "BIGINT DEFAULT 0"},
		// Region key (e.g. "eu-central"). Set via NODE_REGION on the node
		// and reported in the heartbeat. Treated as first-class metadata,
		// orthogonal to tags — tags describe capability/tier, region the
		// physical location for latency-based placement.
		{"nodes", "region", "TEXT DEFAULT ''"},
		// Admin adoption flag. FALSE = node config (name/tags/region) is driven by
		// the heartbeat ENV (automation path); the discovery scan keeps the DB row
		// in sync with the node's env on every beat. TRUE = an admin configured the
		// node via the panel, so the DB values win and the heartbeat env no longer
		// overwrites name/tags/region. Operational fields (status, address, IPs,
		// capacity) always follow the heartbeat regardless.
		{"nodes", "configured", "BOOLEAN NOT NULL DEFAULT FALSE"},
		// BYON multi-tenancy: owner_id NULL = platform node (operator-owned;
		// solo + hoster modes), set = tenant node (the user who brought it).
		// ON DELETE SET NULL so deleting a user reverts their nodes to platform.
		// Only meaningful when feature_byon_enabled; inert otherwise.
		{"nodes", "owner_id", "UUID REFERENCES users(id) ON DELETE SET NULL"},
		// Security questions — JSON array of
		// {question, answer_hash} pairs; answer_hash is bcrypt.
		{"users", "security_questions", "JSONB DEFAULT '[]'"},
		// Server audit — audit_enabled flips on automatically the
		// first time a non-owner is invited (saves space for solo-owner servers).
		// audit_force_on is the admin override for unconditional audit
		// (compliance setups). Effective gate is OR of the two.
		{"servers", "audit_enabled", "BOOLEAN NOT NULL DEFAULT FALSE"},
		{"servers", "audit_force_on", "BOOLEAN NOT NULL DEFAULT FALSE"},
		// Per-server CPU pinning. mode: 'shared' (default, inherit node cpuset /
		// all cores), 'auto' (Core assigns a cpuset spread across least-loaded,
		// P-preferred cores), 'manual' (operator-chosen cpuset). cpuset is the
		// effective core list, e.g. "0-3,8".
		{"servers", "cpu_pinning_mode", "TEXT NOT NULL DEFAULT 'shared'"},
		{"servers", "cpuset", "TEXT NOT NULL DEFAULT ''"},
		// Edge transitional MOTD: what players see (via the gateway edge) while the
		// server is down/starting/migrating. mode: 'auto' (built-in notice, default),
		// 'custom' (edge_motd_text), 'off' (vanilla). Published to Redis for the edge
		// (dylaris:server:<uuid>:edge_motd_mode / :edge_motd_text).
		{"servers", "edge_motd_mode", "TEXT NOT NULL DEFAULT 'auto'"},
		{"servers", "edge_motd_text", "TEXT NOT NULL DEFAULT ''"},
		// Roles + granular capability flags.
		// role values: 'user' | 'support' | 'admin'. is_admin is kept as a
		// derived view for backward compat with handlers that read it
		// directly. support_team is used by Tickets to scope
		// ticket visibility — nullable for everyone else.
		{"users", "role", "VARCHAR(16) NOT NULL DEFAULT 'user'"},
		{"users", "can_delete_servers", "BOOLEAN NOT NULL DEFAULT FALSE"},
		{"users", "can_change_resources", "BOOLEAN NOT NULL DEFAULT FALSE"},
		{"users", "support_team", "VARCHAR(64)"},
		// Per-node encrypted secret. Holds AES-256-GCM ciphertext once provisioned;
		// empty string = no secret yet.
		{"nodes", "node_secret_enc", "TEXT NOT NULL DEFAULT ''"},
		{"nodes", "display_name", "TEXT"},
		// The token of the link the Hub says serves this node (hub:node-link:<nodes.token>,
		// learned by services.NodeLinkLearner). '' = the Hub has not answered, and
		// routes use the derived token. Deliberately NOT link_secret, which
		// link_ownership.go reads with a different meaning.
		{"nodes", "link_token", "TEXT NOT NULL DEFAULT ''"},
		// NOTE: modversions.modrinth_download_url and server_mods.target_dir are
		// deliberately NOT in this set. Their tables are created by later phases,
		// so the ALTER here hits a table that does not exist yet - which is how
		// modrinth_download_url ended up missing on every fresh install. Both are
		// declared in their CREATE TABLE and re-applied for older databases in
		// applyUnifiedModpackSchema and applyPhase10Schema respectively, where
		// the table is guaranteed to exist.
	}
	// The error was discarded here, which is how modversions.modrinth_download_url
	// stayed missing on every fresh install without a word in the log: this loop
	// runs before the modpack tables are created, so its ALTER hit a table that
	// did not exist yet, and the column only appeared on the next boot. Nothing
	// in this set is allowed to fail - ADD COLUMN IF NOT EXISTS is a no-op when
	// the column is already there - so anything that does is a real ordering
	// mistake and has to be visible.
	for _, c := range cols {
		query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", c.table, c.col, c.def)
		if _, err := db.Exec(query); err != nil {
			log.Printf("db: [warn] could not add %s.%s: %v - the column will be missing until the next boot; "+
				"if the table is created later in ensureSchema, declare the column in its CREATE TABLE too",
				c.table, c.col, err)
		}
	}

	// Unique constraints (idempotent)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_name_unique ON nodes (name)`)

	// token is the load-bearing node identity (server-assigned uuid on the
	// hardened path). Enforce uniqueness now that it is authoritative. Existing
	// rows have token=hostname, already kept distinct by idx_nodes_name_unique.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_token_unique ON nodes (token)`); err != nil {
		log.Printf("db: create idx_nodes_token_unique failed (token uniqueness NOT enforced): %v", err)
	}

	// Backfill: existing admins (is_admin=TRUE) get role='admin'
	// so the new role column matches their legacy capability. Idempotent —
	// users that already have a non-default role stay as-is.
	db.Exec(`UPDATE users SET role = 'admin' WHERE is_admin = TRUE AND role = 'user'`)

	return nil
}

// seedSystemModules inserts system modules that don't exist yet and migrates existing ones
func seedSystemModules(db *sql.DB) {
	modules := []struct {
		name, typ, icon, url, role string
		position                   int
		enabled, system            bool
	}{
		{"Servers", "internal", "server", "/servers", "all", 1, true, true},
		{"Admin", "internal", "shield-check", "/admin", "admin", 2, true, true},
		{"Infrastructure", "internal", "cpu", "/infrastructure", "admin", 3, true, true},
		{"Library", "internal", "folder-open", "/library", "admin", 4, false, false},
		// Tickets module, default disabled. Admin opts in from
		// Settings → Modules. Once enabled it appears in the user-facing
		// sidebar via the standard module loader.
		{"Tickets", "internal", "life-buoy", "/tickets", "admin", 5, false, false},
		// Custom tabs across every server the viewer may see, full width.
		// Default disabled like Tickets: it is only useful once the operator
		// has configured a proxy host and someone has actually made a tab.
		{"Custom Tabs", "internal", "layout-dashboard", "/tabs", "all", 6, false, false},
	}
	for _, m := range modules {
		db.Exec(`
			INSERT INTO modules (name, type, icon, url, is_enabled, is_system, position, access_role)
			SELECT $1, $2, $3, $4, $6, $7, $5, $8
			WHERE NOT EXISTS (SELECT 1 FROM modules WHERE name = $1)
		`, m.name, m.typ, m.icon, m.url, m.position, m.enabled, m.system, m.role)
	}

	// Migrate existing deployments: ensure correct positions, icons, system flags
	// AND correct access_role for the admin-only modules so older deployments
	// stop showing the Admin/Infrastructure tabs to regular users.
	db.Exec(`UPDATE modules SET position = 1, is_enabled = TRUE, is_system = TRUE, access_role = 'all' WHERE name = 'Servers'`)
	db.Exec(`INSERT INTO modules (name, type, icon, url, is_enabled, is_system, position, access_role) SELECT 'Admin', 'internal', 'shield-check', '/admin', TRUE, TRUE, 2, 'admin' WHERE NOT EXISTS (SELECT 1 FROM modules WHERE name = 'Admin')`)
	db.Exec(`UPDATE modules SET position = 2, is_enabled = TRUE, is_system = TRUE, access_role = 'admin' WHERE name = 'Admin'`)
	db.Exec(`UPDATE modules SET position = 3, is_enabled = TRUE, is_system = TRUE, icon = 'cpu', access_role = 'admin' WHERE name = 'Infrastructure'`)
	// access_role and position are NOT re-applied for Library and Tickets, and
	// that omission is the point. Settings -> Modules offers a real All/Admin
	// choice for these two - their ENABLED state follows a feature flag, their
	// audience stays the operator's answer to a different question - and a boot
	// that rewrote the column put every such choice back on the next Core
	// restart, silently and with nothing on screen admitting it. Position is the
	// same story: this screen advertises drag-to-reorder.
	//
	// The seed above still sets both, so a fresh install starts where it should;
	// after that the row belongs to the operator.
	db.Exec(`UPDATE modules SET is_system = FALSE, icon = 'folder-open' WHERE name = 'Library'`)
	// Gateway was retired as a standalone module — its UI moved into the
	// Infrastructure module's Routes tab. Drop the row from existing installs.
	db.Exec(`DELETE FROM modules WHERE name = 'Gateway'`)
	// 'Tickets' was previously deleted as a system module — it is now reintroduced
	// as a non-system, opt-in module. Drop only the legacy system row.
	db.Exec(`DELETE FROM modules WHERE name IN ('Console', 'Modpacks', 'Files') AND is_system = TRUE`)
	db.Exec(`DELETE FROM modules WHERE name = 'Tickets' AND is_system = TRUE`)
	// Migrate existing Tickets row if present from prior phase: ensure it keeps
	// the correct icon and non-system flag. Audience and position are left alone
	// for the reason spelled out at the Library row above.
	db.Exec(`UPDATE modules SET icon = 'life-buoy', url = '/tickets', is_system = FALSE WHERE name = 'Tickets'`)

	// Route limits follow the platform limit convention: NULL = no limit, 0 =
	// none, n = the cap. -1 is from the convention BEFORE that one, where a panel
	// toggle wrote -1 for unlimited, and nothing reads it that way any more: a
	// surviving -1 row is now returned as a cap OF -1, and every count is at or
	// over it, so the tenant may hold no addresses and is told they are over a
	// limit of -1.
	db.Exec(`UPDATE gateway_route_limits SET max_routes = NULL WHERE max_routes = -1`)
	// What used to stand here was the opposite direction - SET max_routes = -1
	// WHERE max_routes = 0 - carried over from when 0 meant unlimited. It runs on
	// EVERY boot, not once, so under the current convention it took the one value
	// an operator can enter to say "this account may hold none" and, at the next
	// restart, rewrote it into the broken sentinel above. Removed rather than
	// bounded: the installs it was written for were converted years of releases
	// ago, and a 0 stored today is a deliberate "none".
	// Migrate port:80 → port:443
	db.Exec(`UPDATE gateway_route_limits SET scope = 'port:443' WHERE scope = 'port:80'`)
	// The per_server and port:* scopes were settable in the panel and read by
	// nothing. The controls are gone; the rows go too, so a future feature that
	// reuses either scope name starts from "nobody has decided" rather than
	// silently inheriting a number an operator typed for a cap that never
	// applied.
	db.Exec(`DELETE FROM gateway_route_limits WHERE scope = 'per_server' OR scope LIKE 'port:%'`)
}

// library_disabled stores paths (relative to the library root) that admins
// have hidden from non-admin users. A path is enabled IFF no row exists for
// it AND no row exists for any of its ancestors. Defaulting to "enabled when
// absent" keeps the table small — only the curated exclusions are persisted.
func createLibraryDisabledTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS library_disabled (
		path TEXT PRIMARY KEY,
		disabled_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

func createServerStatsTable(db *sql.DB, useTimescale bool) error {
	query := `CREATE TABLE IF NOT EXISTS server_stats (
		time TIMESTAMPTZ NOT NULL,
		server_uuid TEXT NOT NULL,
		cpu DOUBLE PRECISION,
		cpu_limit DOUBLE PRECISION,
		mem_used BIGINT,
		mem_limit BIGINT,
		players INT,
		max_players INT
	)`
	if _, err := db.Exec(query); err != nil {
		return err
	}

	db.Exec(`CREATE INDEX IF NOT EXISTS idx_server_stats_uuid_time ON server_stats (server_uuid, time DESC)`)

	// Plain-PostgreSQL mode (DB_TYPE=postgres): keep server_stats a regular
	// table. Retention is enforced by the hourly sweep in main.go, so we skip
	// the TimescaleDB calls entirely — no failing hypertable attempt, clean logs.
	if !useTimescale {
		log.Println("DB_TYPE=postgres: server_stats is a plain table (retention via hourly sweep)")
		return nil
	}

	// TimescaleDB mode: promote to a hypertable with native retention. Still
	// non-fatal if the extension happens to be missing (we fall back to the
	// plain table + the hourly sweep), so a mislabelled DB never blocks boot.
	_, err := db.Exec(`SELECT create_hypertable('server_stats', 'time', if_not_exists => TRUE)`)
	if err != nil {
		log.Printf("TimescaleDB hypertable not created (extension may not be installed; falling back to plain table + hourly sweep): %v", err)
	} else {
		log.Println("TimescaleDB hypertable 'server_stats' ready")
		// Attempt retention policy
		_, err = db.Exec(`SELECT add_retention_policy('server_stats', INTERVAL '24 hours', if_not_exists => TRUE)`)
		if err != nil {
			log.Printf("TimescaleDB retention policy not set: %v", err)
		} else {
			log.Println("TimescaleDB 24h retention policy active on server_stats")
		}
	}

	return nil
}

func createGatewayBandwidthStatsTable(db *sql.DB, useTimescale bool) error {
	query := `CREATE TABLE IF NOT EXISTS gateway_bandwidth_stats (
		time      TIMESTAMPTZ NOT NULL,
		component TEXT NOT NULL,
		id        TEXT NOT NULL,
		host      TEXT NOT NULL,
		region    TEXT,
		rx_bps    BIGINT NOT NULL,
		tx_bps    BIGINT NOT NULL,
		cap_mbit  INTEGER NOT NULL
	)`
	if _, err := db.Exec(query); err != nil {
		return err
	}

	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gw_bw_id_time ON gateway_bandwidth_stats (component, id, time DESC)`)

	if !useTimescale {
		log.Println("DB_TYPE=postgres: gateway_bandwidth_stats is a plain table (retention via hourly sweep)")
		return nil
	}

	_, err := db.Exec(`SELECT create_hypertable('gateway_bandwidth_stats', 'time', if_not_exists => TRUE)`)
	if err != nil {
		log.Printf("TimescaleDB hypertable not created for gateway_bandwidth_stats (falling back to plain table + hourly sweep): %v", err)
	} else {
		log.Println("TimescaleDB hypertable 'gateway_bandwidth_stats' ready")
		if _, err := db.Exec(`SELECT add_retention_policy('gateway_bandwidth_stats', INTERVAL '24 hours', if_not_exists => TRUE)`); err != nil {
			log.Printf("TimescaleDB retention policy not set for gateway_bandwidth_stats: %v", err)
		} else {
			log.Println("TimescaleDB 24h retention policy active on gateway_bandwidth_stats")
		}
	}
	return nil
}
